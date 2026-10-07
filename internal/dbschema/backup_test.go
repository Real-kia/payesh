package dbschema

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestBoundedCaptureRefusesSourceGrowthAfterEstimate(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "source.sqlite")
	db, err := sql.Open("sqlite", sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE schema_meta(version INTEGER NOT NULL);INSERT INTO schema_meta VALUES(4);CREATE TABLE evidence(value BLOB);`); err != nil {
		t.Fatal(err)
	}
	var pageSize, pages int64
	if err := db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	budget := (pages + 1) * pageSize
	// An independent writer grows the database after the snapshot-size estimate.
	writer, err := sql.Open("sqlite", sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(`INSERT INTO evidence VALUES(zeroblob(1048576))`); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "bounded.sqlite")
	if err := os.WriteFile(destination, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := boundedSQLiteBackup(t.Context(), db, destination, pageSize, budget); err == nil {
		t.Fatal("capture accepted a source that outgrew its reserved budget")
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > budget {
		t.Fatalf("capture wrote %d bytes beyond %d-byte budget", info.Size(), budget)
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if _, err := os.Stat(destination + suffix); !os.IsNotExist(err) {
			t.Fatalf("unbudgeted capture sidecar %s: %v", suffix, err)
		}
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM evidence`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("failed capture changed source count=%d err=%v", count, err)
	}
}

func TestMigrationBackupPreservesSupportedPageSizesAndWAL(t *testing.T) {
	for _, pageSize := range []int64{512, 4096, 65536} {
		t.Run(strconv.FormatInt(pageSize, 10), func(t *testing.T) {
			root := t.TempDir()
			db, err := sql.Open("sqlite", filepath.Join(root, "source.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`PRAGMA page_size=` + strconv.FormatInt(pageSize, 10) + `;PRAGMA journal_mode=WAL;CREATE TABLE schema_meta(version INTEGER NOT NULL);INSERT INTO schema_meta VALUES(4);CREATE TABLE evidence(value TEXT);INSERT INTO evidence VALUES('committed WAL evidence');`); err != nil {
				t.Fatal(err)
			}
			path, err := CreateMigrationBackup(t.Context(), db, filepath.Join(root, "recovery"), 4, 8<<20)
			if err != nil {
				t.Fatal(err)
			}
			copy, err := sql.Open("sqlite", sqliteReadOnlyURI(path))
			if err != nil {
				t.Fatal(err)
			}
			defer copy.Close()
			var gotPageSize int64
			var evidence string
			if err := copy.QueryRow(`PRAGMA page_size`).Scan(&gotPageSize); err != nil {
				t.Fatal(err)
			}
			if err := copy.QueryRow(`SELECT value FROM evidence`).Scan(&evidence); err != nil {
				t.Fatal(err)
			}
			if gotPageSize != pageSize || evidence != "committed WAL evidence" {
				t.Fatalf("page size=%d evidence=%q", gotPageSize, evidence)
			}
		})
	}
}

func TestBoundedCaptureHonorsCanceledContext(t *testing.T) {
	root := t.TempDir()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE evidence(value TEXT)`); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "copy.sqlite")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := boundedSQLiteBackup(ctx, db, path, 4096, 1<<20); err == nil {
		t.Fatal("capture ignored cancellation")
	}
}

func TestSQLiteIntegrityVerificationAcceptsRestoredOriginalPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restored.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE evidence(value TEXT); INSERT INTO evidence VALUES('restored original')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteBackup(t.Context(), path); err != nil {
		t.Fatalf("integrity verification rejected original restored permissions: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatal("integrity verification changed original restored permissions")
	}
	if err := VerifyPrivateSQLiteBackup(t.Context(), path); err == nil {
		t.Fatal("private migration snapshot verifier accepted public restored mode")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyPrivateSQLiteBackup(t.Context(), path); err != nil {
		t.Fatalf("private verifier rejected private valid snapshot: %v", err)
	}
}
