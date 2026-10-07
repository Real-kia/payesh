package main

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A concurrent reader (for example `payesh cutover status` against a running
// cutover) must make the journal writer wait, not fail with SQLITE_BUSY.
func TestCutoverDatabasesWaitForConcurrentLocksInsteadOfFailing(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "with space #?%", "journal.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	writer, err := openCLIDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.ExecContext(ctx, `CREATE TABLE t (v INTEGER)`); err != nil {
		t.Fatal(err)
	}
	other, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	tx, err := other.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO t VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(400 * time.Millisecond)
		_ = tx.Commit()
	}()
	if _, err := writer.ExecContext(ctx, `INSERT INTO t VALUES (2)`); err != nil {
		t.Fatalf("a write during a short concurrent lock failed instead of waiting: %v", err)
	}
}
