package updater

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	_ "modernc.org/sqlite"
)

func schemaDB(t *testing.T, version int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", t.TempDir()+"/schema.db")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_meta(version INTEGER NOT NULL); INSERT INTO schema_meta VALUES(?)`, version); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSchemaRegistryTransactionalAndBackupGated(t *testing.T) {
	ctx := context.Background()
	db := schemaDB(t, 1)
	registry := SchemaRegistry{MinReadable: 1, Current: 3, Steps: []SchemaStep{
		{From: 1, To: 2, Name: "add-safe", Apply: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `CREATE TABLE safe_value(value TEXT)`)
			return err
		}},
		{From: 2, To: 3, Name: "rewrite", Destructive: true, Apply: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO safe_value VALUES('done')`)
			return err
		}},
	}}
	version, err := registry.RunSchemaMigrations(ctx, db, SchemaMigrationOptions{})
	if err == nil || version != 2 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	var persisted int
	_ = db.QueryRow(`SELECT version FROM schema_meta`).Scan(&persisted)
	if persisted != 2 {
		t.Fatalf("persisted=%d", persisted)
	}
	verified := 0
	version, err = registry.RunSchemaMigrations(ctx, db, SchemaMigrationOptions{BackupVerified: func(context.Context, int, int) error { verified++; return nil }})
	if err != nil || version != 3 || verified != 1 {
		t.Fatalf("version=%d verified=%d err=%v", version, verified, err)
	}
}

func TestSchemaRegistryRollsBackFailedStepAndRejectsVersions(t *testing.T) {
	ctx := context.Background()
	db := schemaDB(t, 1)
	registry := SchemaRegistry{MinReadable: 1, Current: 2, Steps: []SchemaStep{{From: 1, To: 2, Name: "fail", Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, _ = tx.ExecContext(ctx, `CREATE TABLE must_rollback(x)`)
		return errors.New("boom")
	}}}}
	if version, err := registry.RunSchemaMigrations(ctx, db, SchemaMigrationOptions{}); err == nil || version != 1 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	var count int
	_ = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='must_rollback'`).Scan(&count)
	if count != 0 {
		t.Fatal("failed migration was not rolled back")
	}
	if err := registry.CheckSchemaCompatibility(3); err == nil {
		t.Fatal("newer schema accepted")
	}
	if err := (SchemaRegistry{MinReadable: 1, Current: 3, Steps: registry.Steps}).Validate(); err == nil {
		t.Fatal("registry gap accepted")
	}
}

func TestReadSchemaVersionRejectsAmbiguousRows(t *testing.T) {
	db := schemaDB(t, 1)
	if _, err := db.Exec(`INSERT INTO schema_meta(version) VALUES(2)`); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSchemaVersion(t.Context(), db); err == nil {
		t.Fatal("expected multiple schema metadata rows to fail closed")
	}
}

func TestSchemaRegistryRejectsOutOfRangeSteps(t *testing.T) {
	registry := SchemaRegistry{MinReadable: 2, Current: 3, Steps: []SchemaStep{
		{From: 1, To: 2, Name: "obsolete", Apply: func(context.Context, *sql.Tx) error { return nil }},
		{From: 2, To: 3, Name: "current", Apply: func(context.Context, *sql.Tx) error { return nil }},
	}}
	if err := registry.Validate(); err == nil {
		t.Fatal("expected out-of-range migration step to be rejected")
	}
}
