package updater

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

// CutoverPeer is the destination side of a server cutover. A local store
// implements it directly; an authenticated network transport can implement the
// same operations later without changing the orchestration or its recovery.
type CutoverPeer interface {
	// ImportArtifact applies a migration-v2 artifact. previous authorizes a tail
	// reconciliation or a replacement over an earlier verified baseline. An
	// artifact that was already applied succeeds without changing anything.
	ImportArtifact(ctx context.Context, artifact, previous []byte) error
	HasServer(ctx context.Context, serverID contracts.ServerID) (bool, error)
	GetAuthority(ctx context.Context, serverID contracts.ServerID) (monitoring.ServerAuthority, bool, error)
	ActivateAuthority(ctx context.Context, handoff monitoring.AuthorityHandoff) (monitoring.ServerAuthority, error)
}

// LocalStorePeer is a CutoverPeer backed by a destination store in this process.
type LocalStorePeer struct {
	DB    *sql.DB
	Store *monitoring.Store
}

func (p *LocalStorePeer) ImportArtifact(ctx context.Context, artifact, previous []byte) error {
	if p == nil || p.DB == nil {
		return errors.New("updater: destination database is required")
	}
	_, err := ImportMigration(ctx, p.DB, artifact, ImportOptions{PreviousArtifact: previous})
	return err
}

func (p *LocalStorePeer) HasServer(ctx context.Context, serverID contracts.ServerID) (bool, error) {
	if p == nil || p.DB == nil {
		return false, errors.New("updater: destination database is required")
	}
	var count int
	if err := p.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM servers WHERE id=?`, string(serverID)).Scan(&count); err != nil {
		return false, err
	}
	return count == 1, nil
}

func (p *LocalStorePeer) GetAuthority(ctx context.Context, serverID contracts.ServerID) (monitoring.ServerAuthority, bool, error) {
	if p == nil || p.Store == nil {
		return monitoring.ServerAuthority{}, false, errors.New("updater: destination store is required")
	}
	return p.Store.GetServerAuthority(ctx, serverID)
}

func (p *LocalStorePeer) ActivateAuthority(ctx context.Context, handoff monitoring.AuthorityHandoff) (monitoring.ServerAuthority, error) {
	if p == nil || p.Store == nil {
		return monitoring.ServerAuthority{}, errors.New("updater: destination store is required")
	}
	return p.Store.ActivateServerAuthority(ctx, handoff)
}

// StoreCutover orchestrates moving one server's ingest authority from Source to
// a peer. Every hook is idempotent and every exported artifact is persisted
// atomically before it is used, so the journal can re-run any phase after a
// crash. The journal's lease and generation fences already stop two
// coordinators from running a phase at once.
type StoreCutover struct {
	Source   *monitoring.Store
	SourceDB *sql.DB
	Peer     CutoverPeer
	ServerID contracts.ServerID
	StateDir string
	// RevokeSourceIdentity, when set, removes the source's trust in the moved
	// server (for a hub, revoking its node certificate). It runs only after the
	// source has relinquished, and must be idempotent.
	RevokeSourceIdentity func(context.Context, contracts.ServerID) error
}

var (
	cutoverIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{7,127}$`)
	cutoverOwnerFormat = cutoverIDPattern
)

func (c *StoreCutover) validate(req RoleCutoverRequest) error {
	switch {
	case c == nil || c.Source == nil || c.SourceDB == nil || c.Peer == nil:
		return errors.New("updater: store cutover needs a source store, its database and a peer")
	case !validMigrationServerID(c.ServerID):
		return errors.New("updater: store cutover server identifier is invalid")
	case c.StateDir == "":
		return errors.New("updater: store cutover needs a state directory")
	case !cutoverIDPattern.MatchString(req.ID) || !cutoverOwnerFormat.MatchString(req.SourceID) || !cutoverOwnerFormat.MatchString(req.DestinationID) || req.SourceID == req.DestinationID:
		return errors.New("updater: store cutover request identifiers are invalid")
	}
	return validateRoleCutover(req)
}

func (c *StoreCutover) dir(req RoleCutoverRequest) (string, error) {
	dir := filepath.Join(c.StateDir, req.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func (c *StoreCutover) requestDigest(req RoleCutoverRequest) string {
	sum := sha256.Sum256([]byte("payesh-store-cutover-v1\n" + strings.Join([]string{req.ID, string(c.ServerID), req.SourceID, req.DestinationID,
		req.SourceRole, req.DestinationRole, strconv.Itoa(req.ManagedNodes), req.FleetDisposition}, "\n")))
	return hex.EncodeToString(sum[:])
}

func (c *StoreCutover) transition(req RoleCutoverRequest) monitoring.AuthorityTransition {
	return monitoring.AuthorityTransition{ServerID: c.ServerID, CutoverID: req.ID, RequestDigest: c.requestDigest(req)}
}

// writeFileAtomic makes a file durable under its final name only when complete.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+filepath.Base(path)+"."+hex.EncodeToString(suffix)+".tmp")
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func readOptionalFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

// exportOnce returns the persisted artifact for this phase, creating it only the
// first time. Reusing the exact bytes after a crash keeps the destination's
// import journal and baseline checks consistent.
func (c *StoreCutover) exportOnce(ctx context.Context, path string) ([]byte, error) {
	if existing, err := readOptionalFile(path); err != nil || len(existing) > 0 {
		return existing, err
	}
	artifact, err := ExportMigration(ctx, c.SourceDB, ExportOptions{ServerID: c.ServerID, ReplaySafe: true})
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(path, artifact); err != nil {
		return nil, err
	}
	return artifact, nil
}

// Hooks returns the idempotent phase hooks for RunRoleCutover.
func (c *StoreCutover) Hooks() RoleCutoverHooks {
	return RoleCutoverHooks{
		Preflight:            c.preflight,
		Backup:               c.backup,
		Transfer:             c.transfer,
		Verify:               c.verify,
		Freeze:               c.freeze,
		TransferTail:         c.transferTail,
		SwitchAuthority:      c.switchAuthority,
		RevokeOldAuthority:   c.revokeOldAuthority,
		Confirm:              c.confirm,
		Cleanup:              c.cleanup,
		RollbackBeforeSwitch: c.rollbackBeforeSwitch,
	}
}

func (c *StoreCutover) preflight(ctx context.Context, req RoleCutoverRequest) error {
	if err := c.validate(req); err != nil {
		return err
	}
	if _, err := c.dir(req); err != nil {
		return err
	}
	source, found, err := c.Source.GetServerAuthority(ctx, c.ServerID)
	if err != nil {
		return err
	}
	if !found {
		if source, err = c.Source.InitializeServerAuthority(ctx, c.ServerID, req.SourceID); err != nil {
			return err
		}
	}
	if source.Owner != req.SourceID {
		return errors.New("updater: the source server is owned by a different authority")
	}
	if source.State != monitoring.AuthorityActive && source.CutoverID != req.ID {
		return fmt.Errorf("updater: the source server is %s for another cutover", source.State)
	}
	dest, found, err := c.Peer.GetAuthority(ctx, c.ServerID)
	if err != nil {
		return err
	}
	if found && dest.CutoverID != req.ID {
		return errors.New("updater: the destination already holds authority for this server")
	}
	return nil
}

func (c *StoreCutover) backup(ctx context.Context, req RoleCutoverRequest) error {
	if err := c.validate(req); err != nil {
		return err
	}
	dir, err := c.dir(req)
	if err != nil {
		return err
	}
	final := filepath.Join(dir, "source-backup.db")
	if ok, err := backupIsValid(ctx, final, c.ServerID); err != nil || ok {
		return err
	}
	tmp := filepath.Join(dir, "source-backup.db.partial")
	_ = os.Remove(tmp)
	if _, err := c.SourceDB.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		return fmt.Errorf("updater: back up the source store: %w", err)
	}
	if ok, err := backupIsValid(ctx, tmp, c.ServerID); err != nil || !ok {
		_ = os.Remove(tmp)
		return errors.Join(errors.New("updater: the source backup failed verification"), err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// backupIsValid opens a backup read-only, checks integrity and that it holds the server.
func backupIsValid(ctx context.Context, path string, serverID contracts.ServerID) (bool, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return false, err
	}
	defer db.Close()
	var result string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil || result != "ok" {
		return false, err
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM servers WHERE id=?`, string(serverID)).Scan(&count); err != nil {
		return false, err
	}
	return count == 1, nil
}

func (c *StoreCutover) transfer(ctx context.Context, req RoleCutoverRequest) error {
	if err := c.validate(req); err != nil {
		return err
	}
	dir, err := c.dir(req)
	if err != nil {
		return err
	}
	artifact, err := c.exportOnce(ctx, filepath.Join(dir, "initial.artifact"))
	if err != nil {
		return err
	}
	lastPath := filepath.Join(c.StateDir, "last-imported-"+string(c.ServerID)+".artifact")
	previous, err := readOptionalFile(lastPath)
	if err != nil {
		return err
	}
	if err := c.Peer.ImportArtifact(ctx, artifact, previous); err != nil {
		return err
	}
	return writeFileAtomic(lastPath, artifact)
}

func (c *StoreCutover) verify(ctx context.Context, req RoleCutoverRequest) error {
	if err := c.validate(req); err != nil {
		return err
	}
	dir, err := c.dir(req)
	if err != nil {
		return err
	}
	if data, err := readOptionalFile(filepath.Join(dir, "initial.artifact")); err != nil || len(data) == 0 {
		return errors.Join(errors.New("updater: the initial transfer artifact is missing"), err)
	}
	has, err := c.Peer.HasServer(ctx, c.ServerID)
	if err != nil {
		return err
	}
	if !has {
		return errors.New("updater: the destination does not hold the transferred server")
	}
	return nil
}

func (c *StoreCutover) freeze(ctx context.Context, req RoleCutoverRequest) error {
	if err := c.validate(req); err != nil {
		return err
	}
	_, err := c.Source.FreezeServerAuthority(ctx, c.transition(req))
	return err
}

func (c *StoreCutover) transferTail(ctx context.Context, req RoleCutoverRequest) error {
	if err := c.validate(req); err != nil {
		return err
	}
	dir, err := c.dir(req)
	if err != nil {
		return err
	}
	initial, err := readOptionalFile(filepath.Join(dir, "initial.artifact"))
	if err != nil || len(initial) == 0 {
		return errors.Join(errors.New("updater: the initial transfer artifact is missing"), err)
	}
	// The source is frozen, so this export is the final state and never changes.
	tail, err := c.exportOnce(ctx, filepath.Join(dir, "tail.artifact"))
	if err != nil {
		return err
	}
	if err := c.Peer.ImportArtifact(ctx, tail, initial); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(c.StateDir, "last-imported-"+string(c.ServerID)+".artifact"), tail)
}

// expectedSourceGeneration is the generation the source will hold once it
// relinquishes, which the handoff must bind before the source actually does.
func (c *StoreCutover) handoff(ctx context.Context, req RoleCutoverRequest) (monitoring.AuthorityHandoff, error) {
	source, found, err := c.Source.GetServerAuthority(ctx, c.ServerID)
	if err != nil {
		return monitoring.AuthorityHandoff{}, err
	}
	if !found || source.CutoverID != req.ID {
		return monitoring.AuthorityHandoff{}, errors.New("updater: the source is not frozen for this cutover")
	}
	generation := source.Generation
	switch source.State {
	case monitoring.AuthorityFrozen:
		generation++
	case monitoring.AuthorityRelinquished:
	default:
		return monitoring.AuthorityHandoff{}, fmt.Errorf("updater: the source is %s, not frozen", source.State)
	}
	return monitoring.AuthorityHandoff{ServerID: c.ServerID, CutoverID: req.ID, SourceGeneration: generation,
		FrontierDigest: source.FrontierDigest, SourceRequestDigest: c.requestDigest(req), NewOwner: req.DestinationID}, nil
}

func (c *StoreCutover) switchAuthority(ctx context.Context, req RoleCutoverRequest) error {
	if err := c.validate(req); err != nil {
		return err
	}
	handoff, err := c.handoff(ctx, req)
	if err != nil {
		return err
	}
	_, err = c.Peer.ActivateAuthority(ctx, handoff)
	return err
}

func (c *StoreCutover) revokeOldAuthority(ctx context.Context, req RoleCutoverRequest) error {
	if err := c.validate(req); err != nil {
		return err
	}
	handoff, err := c.handoff(ctx, req)
	if err != nil {
		return err
	}
	relinquished, err := c.Source.RelinquishServerAuthority(ctx, c.transition(req))
	if err != nil {
		return err
	}
	if relinquished.Generation != handoff.SourceGeneration {
		return errors.New("updater: the relinquished generation does not match the destination handoff")
	}
	if c.RevokeSourceIdentity != nil {
		if err := c.RevokeSourceIdentity(ctx, c.ServerID); err != nil {
			return fmt.Errorf("updater: revoke the source identity: %w", err)
		}
	}
	return nil
}

func (c *StoreCutover) confirm(ctx context.Context, req RoleCutoverRequest) error {
	if err := c.validate(req); err != nil {
		return err
	}
	source, found, err := c.Source.GetServerAuthority(ctx, c.ServerID)
	if err != nil {
		return err
	}
	if !found || source.State != monitoring.AuthorityRelinquished || source.CutoverID != req.ID {
		return errors.New("updater: the source has not relinquished this server")
	}
	dest, found, err := c.Peer.GetAuthority(ctx, c.ServerID)
	if err != nil {
		return err
	}
	if !found || dest.State != monitoring.AuthorityActive || dest.Owner != req.DestinationID || dest.CutoverID != req.ID ||
		dest.Generation != source.Generation+1 || dest.FrontierDigest != source.FrontierDigest {
		return errors.New("updater: the destination authority does not match the source handoff")
	}
	return nil
}

// cleanup removes only this cutover's transient artifacts; source data and the
// verified backup are never deleted by the orchestrator.
func (c *StoreCutover) cleanup(_ context.Context, req RoleCutoverRequest) error {
	if err := c.validate(req); err != nil {
		return err
	}
	dir, err := c.dir(req)
	if err != nil {
		return err
	}
	for _, name := range []string{"initial.artifact", "tail.artifact"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// rollbackBeforeSwitch resumes the source when a cutover fails before the
// destination takes over. A misconfigured cutover has nothing to undo.
func (c *StoreCutover) rollbackBeforeSwitch(ctx context.Context, req RoleCutoverRequest) error {
	if c.validate(req) != nil {
		return nil
	}
	source, found, err := c.Source.GetServerAuthority(ctx, c.ServerID)
	if err != nil || !found {
		return err
	}
	if source.State != monitoring.AuthorityFrozen || source.CutoverID != req.ID {
		return nil
	}
	_, err = c.Source.AbortServerFreeze(ctx, c.transition(req))
	return err
}

// LoadRoleCutover reports a cutover's journal record. A journal that has never
// recorded a cutover, or an unknown identifier, reports not found.
func LoadRoleCutover(ctx context.Context, journal *sql.DB, id string) (RoleCutoverRecord, bool, error) {
	if journal == nil || !cutoverIDPattern.MatchString(id) {
		return RoleCutoverRecord{}, false, errors.New("updater: a journal and a valid cutover identifier are required")
	}
	var tables int
	if err := journal.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='role_cutovers'`).Scan(&tables); err != nil {
		return RoleCutoverRecord{}, false, err
	}
	if tables == 0 {
		return RoleCutoverRecord{}, false, nil
	}
	return loadCutover(ctx, journal, id)
}
