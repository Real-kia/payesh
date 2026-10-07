package dbschema

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite"
)

// CreateMigrationBackup keeps at most two immutable recovery snapshots. The
// byte budget covers both retained files and the next snapshot; exhausted
// recovery storage fails closed rather than deleting an operator's backup.
func CreateMigrationBackup(ctx context.Context, db *sql.DB, directory string, version int, maxBytes int64) (string, error) {
	// Continuous external writes can repeatedly restart an online backup.
	// Startup must fail closed instead of waiting forever for a quiet source.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if db == nil || maxBytes <= 0 || !filepath.IsAbs(directory) {
		return "", errors.New("database: invalid backup request")
	}
	if err := checkParentSymlinks(directory); err != nil {
		return "", err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("database: backup directory must be private and not a symlink")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return "", err
	}
	var retained int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() {
			return "", errors.New("database: unsafe recovery directory entry")
		}
		if info.Size() < 0 || info.Size() > maxBytes-retained {
			return "", errors.New("database: recovery snapshot byte budget exhausted")
		}
		retained += info.Size()
	}
	if len(entries) >= 2 {
		return "", errors.New("database: recovery snapshot limit reached; preserve or move existing backups before retrying")
	}
	var pages, pageSize int64
	if err := db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return "", err
	}
	if err := db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return "", err
	}
	if pageSize <= 0 || pages < 0 || pages > maxBytes/pageSize || retained > maxBytes || pages*pageSize > maxBytes-retained {
		return "", errors.New("database: recovery snapshot byte budget exhausted")
	}
	file, err := os.CreateTemp(directory, fmt.Sprintf("schema-%d-*.sqlite", version))
	if err != nil {
		return "", err
	}
	destination := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(destination)
		return "", err
	}
	if err := boundedSQLiteBackup(ctx, db, destination, pageSize, maxBytes-retained); err != nil {
		_ = os.Remove(destination)
		return "", err
	}
	info, err = os.Lstat(destination)
	if err != nil {
		return "", err
	}
	// Another connection can grow the source after the initial estimate. Never
	// accept an oversized snapshot as a verified recovery artifact.
	if info.Size() > maxBytes-retained {
		_ = os.Remove(destination)
		return "", errors.New("database: captured snapshot exceeds recovery byte budget")
	}
	backup, err := sql.Open("sqlite", sqliteReadOnlyURI(destination))
	if err != nil {
		return "", err
	}
	defer backup.Close()
	got, err := ReadSchemaVersion(ctx, backup)
	if err != nil {
		return "", err
	}
	if got != version {
		return "", errors.New("database: recovery snapshot schema does not match source")
	}
	return destination, nil
}

// boundedSQLiteBackup applies SQLite's destination page limit while pages are
// copied, so a source growing after the estimate cannot overrun the reserved
// recovery budget. The disposable destination needs no rollback journal; it
// becomes a recovery artifact only after complete copy, verification and sync.
func boundedSQLiteBackup(ctx context.Context, source *sql.DB, destination string, pageSize, maxBytes int64) error {
	if pageSize <= 0 || maxBytes/pageSize < 1 {
		return errors.New("database: recovery snapshot byte budget exhausted")
	}
	if err := checkParentSymlinks(filepath.Dir(destination)); err != nil {
		return err
	}
	info, err := os.Lstat(destination)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("database: unsafe bounded backup destination")
	}
	u := &url.URL{Scheme: "file", Path: destination}
	q := u.Query()
	for _, pragma := range []string{"page_size(" + strconv.FormatInt(pageSize, 10) + ")", "max_page_count(" + strconv.FormatInt(maxBytes/pageSize, 10) + ")", "journal_mode(OFF)", "synchronous(FULL)"} {
		q.Add("_pragma", pragma)
	}
	u.RawQuery = q.Encode()
	conn, err := source.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var timeout int
	if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout=100`); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), fmt.Sprintf("PRAGMA busy_timeout=%d", timeout))
	err = conn.Raw(func(driverConn any) error {
		owner, ok := driverConn.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("database: SQLite driver has no bounded backup API")
		}
		backup, err := owner.NewBackup(u.String())
		if err != nil {
			return err
		}
		for {
			if err := ctx.Err(); err != nil {
				_ = backup.Finish()
				return err
			}
			more, err := backup.Step(64)
			if err != nil {
				_ = backup.Finish()
				return fmt.Errorf("database: bounded SQLite backup: %w", err)
			}
			if !more {
				destinationConn, err := backup.Commit()
				if err != nil {
					return err
				}
				defer destinationConn.Close()
				executor, ok := destinationConn.(sqlite.ExecQuerierContext)
				if !ok {
					return errors.New("database: SQLite destination cannot normalize journal mode")
				}
				// The copied source may carry a WAL header. Normalize it using
				// the still-open destination connection with journaling disabled;
				// subsequent read-only verification must create no sidecars.
				_, err = executor.ExecContext(ctx, "PRAGMA journal_mode=OFF", nil)
				return err
			}
		}
	})
	if err != nil {
		return err
	}
	if err := VerifyPrivateSQLiteBackup(ctx, destination); err != nil {
		return err
	}
	return syncBackup(destination)
}

func sqliteReadOnlyURI(path string) string {
	u := &url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "ro")
	u.RawQuery = q.Encode()
	return u.String()
}

// BackupSQLite creates a consistent single-file snapshot including committed
// WAL data. reservedEmpty permits a caller-owned, securely reserved empty file.
func BackupSQLite(ctx context.Context, source *sql.DB, destination string, reservedEmpty bool) error {
	if source == nil || !filepath.IsAbs(destination) || filepath.Clean(destination) != destination {
		return errors.New("database: invalid SQLite backup request")
	}
	if err := checkParentSymlinks(filepath.Dir(destination)); err != nil {
		return err
	}
	if !reservedEmpty {
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(destination)
			return err
		}
	}
	info, err := os.Lstat(destination)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("database: unsafe SQLite backup destination")
	}
	quoted := strings.ReplaceAll(destination, "'", "''")
	if _, err := source.ExecContext(ctx, "VACUUM INTO '"+quoted+"'"); err != nil {
		_ = os.Remove(destination)
		return fmt.Errorf("database: create SQLite backup: %w", err)
	}
	if err := VerifyPrivateSQLiteBackup(ctx, destination); err != nil {
		_ = os.Remove(destination)
		return err
	}
	return syncBackup(destination)
}

func syncBackup(destination string) error {
	f, err := os.OpenFile(destination, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// VerifyPrivateSQLiteBackup additionally enforces snapshot privacy. Restored
// live databases retain their original permissions and use the integrity-only
// verifier below.
func VerifyPrivateSQLiteBackup(ctx context.Context, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("database: SQLite backup must be a private regular file")
	}
	return VerifySQLiteBackup(ctx, path)
}

// VerifySQLiteBackup validates a regular SQLite file without changing its
// original permissions. It also verifies restored live databases.
func VerifySQLiteBackup(ctx context.Context, path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("database: invalid SQLite backup path")
	}
	if err := checkParentSymlinks(filepath.Dir(path)); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("database: SQLite backup must be a regular file")
	}
	db, err := sql.Open("sqlite", sqliteReadOnlyURI(path))
	if err != nil {
		return err
	}
	defer db.Close()
	var result string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return fmt.Errorf("database: verify SQLite backup: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("database: SQLite backup integrity check failed: %s", result)
	}
	return nil
}

// SnapshotDigest binds the migration gate to the exact verified recovery file.
func SnapshotDigest(path string) ([32]byte, error) {
	var result [32]byte
	f, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return result, err
	}
	copy(result[:], h.Sum(nil))
	return result, nil
}

func checkParentSymlinks(directory string) error {
	for path := filepath.Clean(directory); ; path = filepath.Dir(path) {
		info, err := os.Lstat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			// Darwin provides these two system aliases; all application-owned
			// directory components must remain real directories.
			allowed := false
			if runtime.GOOS == "darwin" && (path == "/var" || path == "/tmp") {
				target, err := filepath.EvalSymlinks(path)
				allowed = err == nil && target == "/private"+path
			}
			if !allowed {
				return errors.New("database: backup path contains a symlink directory")
			}
		}
		if filepath.Dir(path) == path {
			break
		}
	}
	return nil
}
