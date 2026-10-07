package updater

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/trust"
)

const (
	DefaultExecutorReserve  = uint64(64 << 20)
	DefaultArtifactNameHub  = "payesh-server"
	DefaultArtifactNameNode = "payesh-agent"
)

var (
	ErrExecutorConfig     = errors.New("updater: executor configuration is invalid")
	ErrExecutorTarget     = errors.New("updater: target is not this local installation")
	ErrExecutorInProgress = errors.New("updater: another activation is in progress")
)

// BackupFunc is called after the release has been downloaded and staged but
// before activation. A production caller should use BackupSQLite and write
// the supplied path outside the release tree.
type BackupFunc func(context.Context, string) error

// SQLiteBackupFunc adapts the updater's consistent SQLite snapshot primitive
// to ExecutorConfig. The executor still owns the destination naming and the
// caller owns the database handle/lifecycle.
func SQLiteBackupFunc(db *sql.DB) BackupFunc {
	return func(ctx context.Context, destination string) error {
		return BackupSQLite(ctx, db, destination)
	}
}

// ExecutorConfig wires the release trust/source boundary to the existing
// download, archive staging, and activation transaction primitives. Paths are
// intentionally explicit: this executor never accepts a path from a job or
// release manifest.
type ExecutorConfig struct {
	Registry          *trust.Registry
	Source            ReleaseSource
	CurrentCore       string
	AcceptedStatePath string
	ReleaseRoot       string // contains current and versioned release directories
	ActiveDir         string
	JournalPath       string
	DatabasePath      string // explicit local database path; missing file means fresh installation
	BackupDir         string
	LocalServerID     contracts.ServerID
	ArtifactName      string
	GOOS              string
	GOARCH            string
	Download          DownloadOptions
	Backup            BackupFunc
	Health            func(context.Context, string) error
	BackupBytes       uint64
	ReserveBytes      uint64
	Now               func() time.Time
}

// ReleaseExecutor is the concrete local Executor used by a process that owns
// one installation. Remote nodes must be reached through an authenticated
// transport implementation, not by passing remote paths to this type.
type ReleaseExecutor struct{ config ExecutorConfig }

func NewReleaseExecutor(config ExecutorConfig) (*ReleaseExecutor, error) {
	if config.ReserveBytes == 0 {
		config.ReserveBytes = DefaultExecutorReserve
	}
	if config.GOOS == "" {
		config.GOOS = runtime.GOOS
	}
	if config.GOARCH == "" {
		config.GOARCH = runtime.GOARCH
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	return &ReleaseExecutor{config: config}, nil
}

func (c ExecutorConfig) validate() error {
	if c.Registry == nil || c.Source == nil || c.Backup == nil || c.Health == nil || !ValidRelease(c.CurrentCore) {
		return fmt.Errorf("%w: registry, source, backup, health, and current core are required", ErrExecutorConfig)
	}
	for name, value := range map[string]string{
		"database":         c.DatabasePath,
		"accepted state":   c.AcceptedStatePath,
		"release root":     c.ReleaseRoot,
		"active directory": c.ActiveDir,
		"journal":          c.JournalPath,
		"backup directory": c.BackupDir,
	} {
		if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value || value == string(filepath.Separator) {
			return fmt.Errorf("%w: %s path must be a clean absolute path", ErrExecutorConfig, name)
		}
	}
	if filepath.Dir(c.ActiveDir) != filepath.Clean(c.ReleaseRoot) {
		return fmt.Errorf("%w: active directory must be a direct child of release root", ErrExecutorConfig)
	}
	for name, value := range map[string]string{"journal": c.JournalPath, "accepted state": c.AcceptedStatePath} {
		rel, err := filepath.Rel(filepath.Clean(c.ReleaseRoot), value)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%w: %s must be outside release root", ErrExecutorConfig, name)
		}
	}
	if filepath.Clean(c.BackupDir) == filepath.Clean(c.ReleaseRoot) || filepath.Dir(c.BackupDir) == filepath.Clean(c.ReleaseRoot) {
		return fmt.Errorf("%w: backup directory must be outside release root", ErrExecutorConfig)
	}
	if c.LocalServerID == "" || len(c.LocalServerID) < 16 || len(c.LocalServerID) > 128 || strings.ContainsAny(string(c.LocalServerID), "/\\\x00") {
		return fmt.Errorf("%w: local server id is invalid", ErrExecutorConfig)
	}
	if c.ArtifactName != "" && !safeName(c.ArtifactName) {
		return fmt.Errorf("%w: artifact name is invalid", ErrExecutorConfig)
	}
	if c.GOOS == "" || c.GOARCH == "" || !safeName(c.GOOS) || !safeName(c.GOARCH) {
		return fmt.Errorf("%w: target platform is invalid", ErrExecutorConfig)
	}
	return nil
}

func (e *ReleaseExecutor) Execute(ctx context.Context, execution Execution) error {
	if e == nil {
		return fmt.Errorf("%w: executor is nil", ErrExecutorConfig)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if execution.JobID == "" || len(execution.JobID) > 128 || !safeName(execution.JobID) || !ValidRelease(execution.Release) || execution.Target.ID == "" {
		return fmt.Errorf("%w: execution identity is invalid", ErrExecutorConfig)
	}
	if execution.Target.ID != e.config.LocalServerID {
		return &IncompatibleError{Reason: ErrExecutorTarget.Error(), Err: ErrExecutorTarget}
	}
	alreadyCommitted, err := e.recoverOrCheckJournal(execution)
	if err != nil {
		return err
	}
	if alreadyCommitted {
		// Activation and accepted-state persistence are deliberately separate
		// durable boundaries. A crash between them must be repairable without
		// downloading or replacing the already-active release again.
		bundle, fetchErr := e.config.Source.Fetch(ctx, execution.Release)
		if fetchErr != nil {
			if errors.Is(fetchErr, ErrReleaseUnavailable) {
				return &RetryableError{Err: fmt.Errorf("updater: fetch release metadata for committed retry: %w", fetchErr)}
			}
			return fmt.Errorf("updater: fetch release metadata for committed retry: %w", fetchErr)
		}
		if bundle.Manifest.Release != execution.Release {
			return fmt.Errorf("updater: committed release source returned %q for %q", bundle.Manifest.Release, execution.Release)
		}
		state, stateErr := LoadAcceptedState(e.config.AcceptedStatePath)
		if stateErr != nil {
			return stateErr
		}
		if verifyErr := VerifyManifest(e.config.Registry, bundle.Manifest, bundle.Signature, e.config.CurrentCore, state, e.now()); verifyErr != nil {
			return verifyErr
		}
		if saveErr := SaveAcceptedState(e.config.AcceptedStatePath, AcceptedState{Release: execution.Release, CreatedAt: bundle.Manifest.CreatedAt}); saveErr != nil {
			return fmt.Errorf("updater: persist accepted release after committed activation: %w", saveErr)
		}
		return nil
	}
	if reason, code := currentSkipReason(execution.Target, e.now()); reason != "" {
		if code == "offline" {
			return &OfflineError{Reason: reason}
		}
		return &IncompatibleError{Reason: reason}
	}
	bundle, err := e.config.Source.Fetch(ctx, execution.Release)
	if err != nil {
		if errors.Is(err, ErrReleaseUnavailable) {
			return &RetryableError{Err: fmt.Errorf("updater: fetch release metadata: %w", err)}
		}
		return fmt.Errorf("updater: fetch release metadata: %w", err)
	}
	if bundle.Manifest.Release != execution.Release {
		return fmt.Errorf("updater: release source returned %q for %q", bundle.Manifest.Release, execution.Release)
	}
	state, err := LoadAcceptedState(e.config.AcceptedStatePath)
	if err != nil {
		return err
	}
	now := e.now()
	artifactName := e.config.ArtifactName
	if artifactName == "" {
		artifactName = DefaultArtifactNameNode
		if execution.Target.Role == "hub" {
			artifactName = DefaultArtifactNameHub
		}
	}
	artifact, err := SelectArtifact(bundle.Manifest, artifactName, e.config.GOOS, e.config.GOARCH)
	if err != nil {
		return err
	}
	if err := VerifyManifest(e.config.Registry, bundle.Manifest, bundle.Signature, e.config.CurrentCore, state, now); err != nil {
		return err
	}
	if err := CheckCandidateDatabaseSchema(ctx, bundle.Manifest, e.config.DatabasePath); err != nil {
		return &IncompatibleError{Reason: err.Error(), Err: err}
	}
	jobDir := filepath.Join(e.config.ReleaseRoot, ".downloads", execution.JobID)
	archivePath := filepath.Join(jobDir, "artifact.tar.gz")
	if err := e.obtainArchive(ctx, bundle, artifact, state, now, archivePath); err != nil {
		return err
	}
	candidate := filepath.Join(e.config.ReleaseRoot, execution.Release)
	// A crash after StageArtifact leaves a candidate. Rebuild it from the
	// verified archive so a retry never trusts mutable candidate contents.
	if _, statErr := os.Stat(candidate); statErr == nil {
		if err := removeScoped(candidate, e.config.ReleaseRoot); err != nil {
			return err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if err := StageArtifact(artifact, archivePath, candidate, e.config.BackupBytes, e.config.ReserveBytes); err != nil {
		return fmt.Errorf("updater: stage release %s: %w", execution.Release, err)
	}
	if e.config.Backup != nil {
		backupPath := filepath.Join(e.config.BackupDir, execution.JobID+"-"+execution.Release+".sqlite")
		if err := e.config.Backup(ctx, backupPath); err != nil {
			return fmt.Errorf("updater: create pre-activation backup: %w", err)
		}
	}
	tx := Transaction{JournalPath: e.config.JournalPath, ActiveDir: e.config.ActiveDir, StagedDir: candidate, Release: execution.Release, Health: e.config.Health, Now: e.config.Now}
	if err := tx.Activate(ctx); err != nil {
		return err
	}
	if err := SaveAcceptedState(e.config.AcceptedStatePath, AcceptedState{Release: execution.Release, CreatedAt: bundle.Manifest.CreatedAt}); err != nil {
		return fmt.Errorf("updater: persist accepted release: %w", err)
	}
	if err := os.RemoveAll(jobDir); err != nil {
		return fmt.Errorf("updater: clean downloaded artifact: %w", err)
	}
	if err := syncDirectory(e.config.ReleaseRoot); err != nil {
		return fmt.Errorf("updater: sync release root after activation: %w", err)
	}
	return nil
}

func (e *ReleaseExecutor) obtainArchive(ctx context.Context, bundle ReleaseBundle, artifact contracts.ReleaseArtifact, state AcceptedState, now time.Time, destination string) error {
	// Verify the signed metadata on every retry, including when a previously
	// downloaded archive is reused. A cached byte stream must never become a
	// way to bypass key, replay, or minimum-core policy.
	if err := VerifyManifest(e.config.Registry, bundle.Manifest, bundle.Signature, e.config.CurrentCore, state, now); err != nil {
		return err
	}
	if info, err := os.Lstat(destination); err == nil && info.Mode().IsRegular() {
		if err := VerifyArtifactFile(artifact, destination); err == nil {
			return nil
		}
		if err := removeScoped(destination, filepath.Dir(destination)); err != nil {
			return err
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	err := DownloadArtifact(ctx, artifact, destination, DownloadOptions{Client: e.config.Download.Client, Timeout: e.config.Download.Timeout, MaxBytes: e.config.Download.MaxBytes, RequiredFree: e.config.Download.RequiredFree, ReplaceExisting: true})
	if err != nil {
		return fmt.Errorf("updater: download release artifact: %w", err)
	}
	return nil
}

func (e *ReleaseExecutor) recoverOrCheckJournal(execution Execution) (bool, error) {
	j, err := LoadJournal(e.config.JournalPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if j.ActiveDir != e.config.ActiveDir {
		return false, ErrExecutorInProgress
	}
	if j.Release == execution.Release && j.Phase == PhaseCommitted {
		return true, nil
	}
	if j.Phase == PhasePrepared || j.Phase == PhasePreserved || j.Phase == PhaseActivated {
		if j.Release != execution.Release {
			return false, ErrExecutorInProgress
		}
		if recoverErr := Recover(e.config.JournalPath, e.now()); recoverErr != nil {
			updated, loadErr := LoadJournal(e.config.JournalPath)
			if loadErr != nil || updated.Phase != PhaseRolledBack {
				return false, fmt.Errorf("updater: recover interrupted activation: %w", recoverErr)
			}
		}
	}
	return false, nil
}

func (e *ReleaseExecutor) now() time.Time {
	if e.config.Now != nil {
		return e.config.Now().UTC()
	}
	return time.Now().UTC()
}

func safeName(name string) bool {
	return name != "" && filepath.Base(name) == name && name != "." && name != ".." && !strings.ContainsAny(name, "/\\") && !strings.ContainsRune(name, '\x00')
}
