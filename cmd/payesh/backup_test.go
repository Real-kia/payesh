package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestPayeshBackupAndVerify(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	sourceDBPath := filepath.Join(dir, "source.db")
	backupDBPath := filepath.Join(dir, "backup.db")

	db, err := sql.Open("sqlite", sourceDBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE test_data (id INTEGER PRIMARY KEY, msg TEXT); INSERT INTO test_data (msg) VALUES ('online-backup-test');`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	if err := backupDatabase(ctx, []string{"-db", sourceDBPath, "-output", backupDBPath}); err != nil {
		t.Fatalf("backupDatabase failed: %v", err)
	}

	if _, err := os.Stat(backupDBPath); err != nil {
		t.Fatalf("expected backup file to exist: %v", err)
	}

	if err := verifyBackup(ctx, []string{"-file", backupDBPath}); err != nil {
		t.Fatalf("verifyBackup failed: %v", err)
	}

	// Verify content is queryable
	backupDB, err := sql.Open("sqlite", "file:"+backupDBPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer backupDB.Close()

	var msg string
	if err := backupDB.QueryRowContext(ctx, "SELECT msg FROM test_data WHERE id=1").Scan(&msg); err != nil || msg != "online-backup-test" {
		t.Fatalf("unexpected value from restored database: %q, err: %v", msg, err)
	}
}
