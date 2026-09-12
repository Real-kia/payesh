package updater

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupSQLiteIncludesCommittedWALAndRefusesOverwrite(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "source.db")
	db, err := sql.Open("sqlite", sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE evidence(value TEXT); INSERT INTO evidence VALUES ('durable');`); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(root, "backups", "before.db")
	if err := BackupSQLite(context.Background(), db, backup); err != nil {
		t.Fatal(err)
	}
	copyDB, err := sql.Open("sqlite", "file:"+backup+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	var value string
	if err := copyDB.QueryRow(`SELECT value FROM evidence`).Scan(&value); err != nil || value != "durable" {
		t.Fatalf("value=%q err=%v", value, err)
	}
	if err := BackupSQLite(context.Background(), db, backup); err == nil {
		t.Fatal("existing backup overwritten")
	}
	info, err := os.Stat(backup)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v", info.Mode().Perm())
	}
}

func TestVerifySQLiteBackupRejectsCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.db")
	if err := os.WriteFile(path, []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteBackup(context.Background(), path); err == nil {
		t.Fatal("corrupt backup accepted")
	}
}
