package updater

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// BackupSQLite creates a consistent single-file snapshot using SQLite's
// VACUUM INTO facility, which includes committed WAL contents. The destination
// must not exist; callers keep it outside the candidate release directory.
func BackupSQLite(ctx context.Context, source *sql.DB, destination string) error {
	if source == nil || !filepath.IsAbs(destination) || filepath.Clean(destination) != destination {
		return errors.New("updater: invalid SQLite backup request")
	}
	if _, err := os.Stat(destination); err == nil {
		return errors.New("updater: SQLite backup destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	// SQLite does not accept a bound parameter for VACUUM INTO in every
	// supported build. Quote a validated absolute path as a SQL string literal.
	quoted := strings.ReplaceAll(destination, "'", "''")
	if _, err := source.ExecContext(ctx, "VACUUM INTO '"+quoted+"'"); err != nil {
		_ = os.Remove(destination)
		return fmt.Errorf("updater: create SQLite backup: %w", err)
	}
	if err := os.Chmod(destination, 0o600); err != nil {
		_ = os.Remove(destination)
		return err
	}
	if err := VerifySQLiteBackup(ctx, destination); err != nil {
		_ = os.Remove(destination)
		return err
	}
	return nil
}

func VerifySQLiteBackup(ctx context.Context, path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("updater: invalid SQLite backup path")
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return fmt.Errorf("updater: verify SQLite backup: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("updater: SQLite backup integrity check failed: %s", result)
	}
	return nil
}
