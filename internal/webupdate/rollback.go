package webupdate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/Real-kia/payesh/internal/updater"
)

// InstallationTransaction journals the installed-layout activation boundary.
// Its root is local configuration only. Remote intent cannot select paths.
type InstallationTransaction struct {
	Root               string
	JobID              string
	SnapshotLimitBytes uint64
	StorageBudgetBytes uint64
	AvailableBytes     func(string) (uint64, error)
	beforeSnapshotOpen func(string)
	Services           func(context.Context, string, string, string) error // action, role, init
}

type installationEntry struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	Mode   uint32 `json:"mode"`
	UID    int    `json:"uid"`
	GID    int    `json:"gid"`
}
type installationJournal struct {
	JobID   string              `json:"job_id"`
	Phase   string              `json:"phase"`
	Role    string              `json:"role"`
	Init    string              `json:"init"`
	Entries []installationEntry `json:"entries"`
}

func installationPaths() []string {
	paths := []string{"/etc/payesh", InstallationScopePath, "/usr/share/payesh/web-assets", "/var/lib/payesh/install-state.json", "/var/lib/payesh/payesh.db"}
	for _, name := range []string{"payesh", "payesh-agent", "payesh-server", "payesh-install", "payesh-privd", "payesh-updater-watchdog"} {
		paths = append(paths, "/usr/bin/"+name)
	}
	for _, name := range []string{"payesh-agent", "payesh-server", "payesh-privd", "payesh-updater-watchdog", "payesh-update"} {
		paths = append(paths, "/etc/systemd/system/"+name+".service", "/etc/init.d/"+name)
	}
	return paths
}
func (t InstallationTransaction) path(path string) string {
	return filepath.Join(t.Root, strings.TrimPrefix(path, "/"))
}
func (t InstallationTransaction) dir() string {
	return t.path("/var/backups/payesh/" + strings.TrimSuffix(fleetResultName(t.JobID), ".json"))
}

const defaultSnapshotLimit = uint64(2 << 30)
const defaultSnapshotStorageBudget = uint64(8 << 30)
const snapshotReserve = uint64(64 << 20)

func (t InstallationTransaction) limits() (uint64, uint64) {
	limit, budget := t.SnapshotLimitBytes, t.StorageBudgetBytes
	if limit == 0 {
		limit = defaultSnapshotLimit
	}
	if budget == 0 {
		budget = defaultSnapshotStorageBudget
	}
	return limit, budget
}
func (t InstallationTransaction) validate() error {
	if t.Root == "" || !filepath.IsAbs(t.Root) || filepath.Clean(t.Root) != t.Root || t.Services == nil || !validFleetRequest(Request{JobID: t.JobID, Version: "1.0.0", Deadline: time.Now()}) {
		return errors.New("invalid installation rollback configuration")
	}
	limit, budget := t.limits()
	if limit > defaultSnapshotLimit || budget > defaultSnapshotStorageBudget {
		return errors.New("snapshot limits may only tighten the installed defaults")
	}
	return nil
}
func safeDirectory(path string, mode os.FileMode, privileged bool) error {
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err = os.Mkdir(current, mode); err != nil {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if !privileged && trustedPlatformAlias(current) {
			continue
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("rollback directory must not contain symlinks")
		}
		// The final backup subtree is privileged; normal system parents such as
		// /var and /usr may be accessible, but must not be writable by others.
		if privileged {
			if stat, ok := info.Sys().(*syscall.Stat_t); ok && (stat.Uid != 0 || info.Mode().Perm()&0022 != 0) {
				return errors.New("rollback directory is not controlled by root")
			}
		}
	}
	return os.Chmod(path, mode)
}
func (t InstallationTransaction) write(j installationJournal) error {
	return writeJSON(t.dir(), "journal.json", j, 0600)
}
func (t InstallationTransaction) load() (installationJournal, error) {
	var j installationJournal
	if err := t.validate(); err != nil {
		return j, err
	}
	if err := safeExistingParents(t.dir(), t.Root == "/"); err != nil {
		return j, err
	}
	data, err := os.ReadFile(filepath.Join(t.dir(), "journal.json"))
	if err != nil {
		return j, err
	}
	if len(data) > 32768 {
		return j, errors.New("rollback journal exceeds limit")
	}
	if err = json.Unmarshal(data, &j); err != nil {
		return j, err
	}
	if j.JobID != t.JobID || len(j.Entries) != len(installationPaths()) {
		return j, errors.New("rollback journal identity or scope is invalid")
	}
	paths := installationPaths()
	for i, e := range j.Entries {
		if e.Path != paths[i] {
			return j, errors.New("rollback journal contains an unmanaged path")
		}
	}
	if (j.Role != "hub" && j.Role != "standalone" && j.Role != "node") || (j.Init != "systemd" && j.Init != "openrc") {
		return j, errors.New("rollback journal service scope is invalid")
	}
	return j, nil
}
func safeExistingParents(path string, privileged bool) error {
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if !privileged && trustedPlatformAlias(current) {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("rollback path contains symlink")
		}
		if privileged {
			if stat, ok := info.Sys().(*syscall.Stat_t); ok && (stat.Uid != 0 || info.Mode().Perm()&0022 != 0) {
				return errors.New("rollback path is not controlled by root")
			}
		}
	}
	return nil
}
func fileOwner(info os.FileInfo) (int, int) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(stat.Uid), int(stat.Gid)
	}
	return -1, -1
}

// Prepare stops application services before snapshotting so all copied state
// describes one quiescent installation. SQLite VACUUM includes committed WAL.
func (t InstallationTransaction) Prepare(ctx context.Context) (resultErr error) {
	if err := t.validate(); err != nil {
		return err
	}
	if err := safeDirectory(t.dir(), 0700, t.Root == "/"); err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(t.dir(), "journal.json")); err == nil {
		return errors.New("installation snapshot already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	state, err := ReadInstallationScope(t.Root)
	if err != nil {
		return err
	}
	if state.Role == "cli-only" {
		return errors.New("CLI role cannot receive fleet activation")
	}
	if err = t.preflightStorage(); err != nil {
		return err
	}
	remaining, _ := t.limits()
	j := installationJournal{JobID: t.JobID, Phase: "capturing", Role: state.Role, Init: state.Init}
	for _, path := range installationPaths() {
		j.Entries = append(j.Entries, installationEntry{Path: path})
	}
	if err = t.write(j); err != nil {
		return err
	}
	prepared := false
	defer func() {
		if !prepared {
			restartCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if restartErr := t.Services(restartCtx, "start", j.Role, j.Init); restartErr != nil {
				resultErr = fmt.Errorf("snapshot failed: %w; restarting previous services failed: %v", resultErr, restartErr)
				return
			}
			j.Phase = "rolled-back"
			if journalErr := t.write(j); journalErr != nil {
				resultErr = fmt.Errorf("snapshot failed: %w; recording recovery failed: %v", resultErr, journalErr)
				return
			}
			// Capturing failed before installation mutation. Once old services
			// restart and recovery is durable, incomplete payloads are dispensable.
			if cleanupErr := os.RemoveAll(filepath.Join(t.dir(), "files")); cleanupErr != nil {
				resultErr = fmt.Errorf("snapshot failed: %w; partial snapshot cleanup failed: %v", resultErr, cleanupErr)
			}
		}
	}()
	if err = t.Services(ctx, "stop", j.Role, j.Init); err != nil {
		return err
	}
	for i, path := range installationPaths() {
		source := t.path(path)
		info, err := os.Lstat(source)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return errors.New("managed snapshot path must be a regular file or directory")
		}
		uid, gid := fileOwner(info)
		j.Entries[i] = installationEntry{Path: path, Exists: true, Mode: uint32(info.Mode().Perm()), UID: uid, GID: gid}
		dest := filepath.Join(t.dir(), "files", strings.TrimPrefix(path, "/"))
		if err = os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			return err
		}
		if path == "/var/lib/payesh/payesh.db" {
			// Copy through a confined file handle while application writers are
			// stopped. SQLite itself opens only the protected snapshot, never a
			// service-writable source path that could be exchanged for a symlink.
			raw := dest + ".raw"
			if err = copyInstallationTreeBounded(source, raw, &remaining); err != nil {
				return err
			}
			if _, walErr := os.Lstat(source + "-wal"); walErr == nil {
				if err = copyInstallationTreeBounded(source+"-wal", raw+"-wal", &remaining); err != nil {
					return err
				}
			} else if !errors.Is(walErr, os.ErrNotExist) {
				return walErr
			}
			db, err := sql.Open("sqlite", "file:"+raw+"?mode=rw")
			if err != nil {
				return err
			}
			// Concurrent growth after preflight must fit fresh headroom before
			// VACUUM temporarily holds raw+WAL and the consistent output.
			rawBytes, sizeErr := snapshotTreeBytes(raw, defaultSnapshotLimit)
			if sizeErr != nil {
				_ = db.Close()
				return sizeErr
			}
			if walBytes, walErr := snapshotTreeBytes(raw+"-wal", defaultSnapshotLimit); walErr == nil {
				rawBytes += walBytes
			} else if !errors.Is(walErr, os.ErrNotExist) {
				_ = db.Close()
				return walErr
			}
			available, spaceErr := t.available(t.dir())
			if spaceErr != nil || available < rawBytes+snapshotReserve {
				_ = db.Close()
				return errors.New("insufficient headroom for consistent database snapshot")
			}
			err = updater.BackupSQLite(ctx, db, dest)
			closeErr := db.Close()
			for _, suffix := range []string{"", "-wal", "-shm"} {
				_ = os.Remove(raw + suffix)
			}
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
			vacuumInfo, statErr := os.Stat(dest)
			if statErr != nil {
				return statErr
			}
			if uint64(vacuumInfo.Size()) > remaining {
				return errors.New("snapshot exceeds managed size limit")
			}
			remaining -= uint64(vacuumInfo.Size())
			if err = os.Chmod(dest, info.Mode().Perm()); err != nil {
				return err
			}
			if os.Geteuid() == 0 {
				if err = os.Chown(dest, uid, gid); err != nil {
					return err
				}
			}
		} else if err = copyInstallationTreeBounded(source, dest, &remaining, t.beforeSnapshotOpen); err != nil {
			return err
		}
	}
	j.Phase = "prepared"
	if err = t.write(j); err != nil {
		return err
	}
	prepared = true
	return nil
}

func copyInstallationTree(source, destination string) error {
	remaining := defaultSnapshotLimit
	return copyInstallationTreeBounded(source, destination, &remaining)
}
func copyInstallationTreeBounded(source, destination string, remaining *uint64, hooks ...func(string)) error {
	// Retain the original fixed-tree parent descriptor for the entire walk.
	root, err := os.OpenRoot(filepath.Dir(source))
	if err != nil {
		return err
	}
	defer root.Close()
	var hook func(string)
	if len(hooks) > 0 {
		hook = hooks[0]
	}
	entries := 0
	return copyInstallationRoot(root, filepath.Base(source), source, destination, remaining, &entries, hook)
}

func copyInstallationRoot(root *os.Root, name, source, destination string, remaining *uint64, entriesSeen *int, hook func(string)) error {
	*entriesSeen++
	if *entriesSeen > 10000 {
		return errors.New("snapshot tree exceeds entry limit")
	}
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("managed tree contains symlink")
	}
	if hook != nil {
		hook(source)
	}
	current, err := root.Lstat(name)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, current) {
		return errors.New("managed snapshot path changed while opening")
	}
	if info.IsDir() {
		child, err := root.OpenRoot(name)
		if err != nil {
			return err
		}
		defer child.Close()
		directory, err := child.Open(".")
		if err != nil {
			return err
		}
		defer directory.Close()
		opened, err := directory.Stat()
		if err != nil || !os.SameFile(info, opened) {
			return errors.New("managed snapshot directory changed while opening")
		}
		if *remaining < 4096 {
			return errors.New("snapshot exceeds managed size limit")
		}
		*remaining -= 4096
		if err = os.Mkdir(destination, 0700); err != nil {
			return err
		}
		// Enumerate the retained directory handle, never its mutable pathname.
		for {
			entries, readErr := directory.ReadDir(128)
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return readErr
			}
			for _, entry := range entries {
				if err = copyInstallationRoot(child, entry.Name(), filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name()), remaining, entriesSeen, hook); err != nil {
					return err
				}
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
		}

	} else {
		if !info.Mode().IsRegular() {
			return errors.New("managed tree contains special file")
		}
		input, err := root.Open(name)
		if err != nil {
			return err
		}
		defer input.Close()
		opened, err := input.Stat()
		if err != nil || !os.SameFile(info, opened) {
			return errors.New("managed snapshot file changed while opening")
		}
		output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(snapshotBoundedWriter{output, remaining}, input)
		syncErr := output.Sync()
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	uid, gid := fileOwner(info)
	if os.Geteuid() == 0 {
		if err = os.Chown(destination, uid, gid); err != nil {
			return err
		}
	}
	if err = os.Chmod(destination, info.Mode().Perm()); err != nil {
		return err
	}
	// File contents, final metadata and directory entries must survive before
	// a journal can declare this tree restored. Sync directories bottom-up.
	return syncInstallationPath(destination)
}

func syncInstallationPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (t InstallationTransaction) syncRestoredParents() error {
	seen := map[string]bool{}
	for _, path := range installationPaths() {
		for parent := filepath.Dir(t.path(path)); ; parent = filepath.Dir(parent) {
			if !seen[parent] {
				if err := syncInstallationPath(parent); err != nil {
					return err
				}
				seen[parent] = true
			}
			if parent == t.Root {
				break
			}
		}
	}
	return nil
}

func (t InstallationTransaction) MarkActivating() error {
	j, err := t.load()
	if err != nil {
		return err
	}
	if j.Phase != "prepared" {
		return errors.New("snapshot is not prepared")
	}
	j.Phase = "activating"
	return t.write(j)
}
func (t InstallationTransaction) Commit() error {
	j, err := t.load()
	if err != nil {
		return err
	}
	if j.Phase != "activating" {
		return errors.New("snapshot activation is not in progress")
	}
	j.Phase = "committed"
	return t.write(j)
}

// Recover rolls back every uncommitted activation after worker interruption.
// Capturing snapshots changed no installed files; restart the old services.
func (t InstallationTransaction) Recover(ctx context.Context) (bool, error) {
	j, err := t.load()
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch j.Phase {
	case "committed":
		return false, nil
	case "rolled-back":
		return true, nil
	case "capturing":
		if err = t.Services(ctx, "start", j.Role, j.Init); err != nil {
			return false, err
		}
		j.Phase = "rolled-back"
		return true, t.write(j)
	case "prepared", "activating", "preserving", "restoring", "restarting":
		return true, t.Rollback(ctx)
	default:
		return false, errors.New("invalid rollback journal phase")
	}
}
func (t InstallationTransaction) Rollback(ctx context.Context) error {
	j, err := t.load()
	if err != nil {
		return err
	}
	if j.Phase == "committed" {
		return errors.New("committed installation cannot be rolled back by recovery")
	}
	if j.Phase == "rolled-back" {
		return nil
	}
	if j.Phase == "capturing" {
		if err = t.Services(ctx, "start", j.Role, j.Init); err != nil {
			return err
		}
		j.Phase = "rolled-back"
		return t.write(j)
	}
	if j.Phase == "restarting" {
		// Previous services may already be running and accepting samples. Never
		// stop them or copy the old snapshot over their newly accepted data.
		if err = t.preserveFailedGeneration(ctx, false); err != nil {
			return err
		}
		return t.restartRestoredServices(ctx, j)
	}
	if j.Phase != "prepared" && j.Phase != "activating" && j.Phase != "preserving" && j.Phase != "restoring" {
		return errors.New("invalid rollback journal phase")
	}
	alreadyRestoring := j.Phase == "restoring"
	if !alreadyRestoring {
		j.Phase = "preserving"
	}
	if err = t.write(j); err != nil {
		return err
	}
	if err = t.Services(ctx, "stop", j.Role, j.Init); err != nil {
		return err
	}
	if err = t.preserveFailedGeneration(ctx, !alreadyRestoring); err != nil {
		return fmt.Errorf("preserving failed generation before restore: %w", err)
	}
	j.Phase = "restoring"
	if err = t.write(j); err != nil {
		return err
	}
	for _, entry := range j.Entries {
		if err = ctx.Err(); err != nil {
			return err
		}
		dest := t.path(entry.Path)
		// Parent paths are fixed installation locations. A planted final symlink
		// is replaced rather than followed; a symlink parent fails closed.
		if err = safeExistingParents(filepath.Dir(dest), false); errors.Is(err, os.ErrNotExist) {
			if err = os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if entry.Path == "/var/lib/payesh/payesh.db" {
			for _, suffix := range []string{"-wal", "-shm"} {
				if err = os.Remove(dest + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
		}
		if !entry.Exists {
			if err = os.RemoveAll(dest); err != nil {
				return err
			}
			continue
		}
		source := filepath.Join(t.dir(), "files", strings.TrimPrefix(entry.Path, "/"))
		if entry.Path == "/var/lib/payesh/payesh.db" {
			if err = updater.VerifySQLiteBackup(ctx, source); err != nil {
				return err
			}
			for _, suffix := range []string{"-wal", "-shm"} {
				if err = os.Remove(dest + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
		}
		temp, err := os.MkdirTemp(filepath.Dir(dest), ".payesh-restore-")
		if err != nil {
			return err
		}
		staged := filepath.Join(temp, "restored")
		if err = copyInstallationTree(source, staged); err != nil {
			_ = os.RemoveAll(temp)
			return err
		}
		if err = os.RemoveAll(dest); err != nil {
			_ = os.RemoveAll(temp)
			return err
		}
		if err = os.Rename(staged, dest); err != nil {
			_ = os.RemoveAll(temp)
			return err
		}
		_ = os.RemoveAll(temp)
	}
	if err = t.syncRestoredParents(); err != nil {
		return err
	}
	// Durably close the file-restoration boundary before any service can
	// write fresh data. Retry after partial starts only resumes service start.
	j.Phase = "restarting"
	if err = t.write(j); err != nil {
		return err
	}
	return t.restartRestoredServices(ctx, j)
}

func (t InstallationTransaction) restartRestoredServices(ctx context.Context, j installationJournal) error {
	if err := t.Services(ctx, "reload", j.Role, j.Init); err != nil {
		return err
	}
	if err := t.Services(ctx, "start", j.Role, j.Init); err != nil {
		return err
	}
	j.Phase = "rolled-back"
	return t.write(j)
}

func (t InstallationTransaction) Phase() (string, error) { j, err := t.load(); return j.Phase, err }

// ErrRollbackIncomplete retains worker intent for recovery when restoring the
// previous installation did not finish. Original activation errors stay wrapped.
var ErrRollbackIncomplete = errors.New("installation rollback requires recovery")
var ErrInstallationRestored = errors.New("previous installation restored")

func InstallationRollbackError(cause, restore error) error {
	if restore != nil {
		return fmt.Errorf("%w: activation failed: %w; restore failed: %v", ErrRollbackIncomplete, cause, restore)
	}
	return fmt.Errorf("%w: activation failed: %w", ErrInstallationRestored, cause)
}

// macOS exposes its system-owned temporary roots through these fixed aliases.
// Fixture checks still reject every symlink inside the actual target tree.
func trustedPlatformAlias(path string) bool {
	if runtime.GOOS != "darwin" || (path != "/var" && path != "/tmp") {
		return false
	}
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != 0 {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	return err == nil && resolved == "/private"+path
}

// Retained snapshots use a fixed storage budget and fail closed when full.
// Operators may remove terminal snapshots; active or unresolved journals are
// never deleted by this worker.
func snapshotTreeBytes(path string, limit uint64) (uint64, error) {
	var total uint64
	entries := 0
	err := filepath.WalkDir(path, func(_ string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		entries++
		if entries > 10000 {
			return errors.New("snapshot tree exceeds entry limit")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return errors.New("snapshot tree contains unsafe entry")
		}
		size := uint64(4096)
		if !info.IsDir() {
			size = uint64(info.Size())
		}
		if size > limit || total > limit-size {
			return errors.New("snapshot exceeds managed storage limit")
		}
		total += size
		return nil
	})
	return total, err
}
func (t InstallationTransaction) available(path string) (uint64, error) {
	if t.AvailableBytes != nil {
		return t.AvailableBytes(path)
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}
func (t InstallationTransaction) preflightStorage() error {
	limit, budget := t.limits()
	var source, largest, database uint64
	for _, path := range installationPaths() {
		size, err := snapshotTreeBytes(t.path(path), limit)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if source > limit-size {
			return errors.New("snapshot exceeds managed size limit")
		}
		source += size
		if size > largest {
			largest = size
		}
		if path == "/var/lib/payesh/payesh.db" {
			database = size
			wal, err := snapshotTreeBytes(t.path(path)+"-wal", limit)
			if err == nil {
				database += wal
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	// Database copy, WAL and VACUUM output coexist during preparation. Restore
	// stages the largest fixed managed artifact before replacing the candidate.
	required := source + 2*database + largest + snapshotReserve
	retained, err := snapshotTreeBytes(filepath.Dir(t.dir()), budget)
	if err != nil {
		return err
	}
	if required > budget || retained > budget-required {
		return errors.New("snapshot archive budget full; clean completed snapshots before retrying")
	}
	available, err := t.available(t.dir())
	if err != nil {
		return err
	}
	if available < required {
		return errors.New("insufficient free space for snapshot and rollback staging")
	}
	return nil
}

type snapshotBoundedWriter struct {
	writer    io.Writer
	remaining *uint64
}

func (w snapshotBoundedWriter) Write(data []byte) (int, error) {
	if uint64(len(data)) > *w.remaining {
		if *w.remaining == 0 {
			return 0, errors.New("snapshot exceeds managed size limit")
		}
		allowed := int(*w.remaining)
		n, err := w.writer.Write(data[:allowed])
		*w.remaining -= uint64(n)
		if err != nil {
			return n, err
		}
		return n, errors.New("snapshot exceeds managed size limit")
	}
	n, err := w.writer.Write(data)
	*w.remaining -= uint64(n)
	return n, err
}
