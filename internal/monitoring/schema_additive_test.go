package monitoring

import (
	"context"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every entry must stay additive, or an older binary could misread the data.
func TestAdditiveSchemaEntriesAreAdditive(t *testing.T) {
	for _, table := range additiveTables {
		if !strings.HasPrefix(table.ddl, "CREATE TABLE IF NOT EXISTS "+table.name+" ") {
			t.Errorf("table %s: ddl must start with CREATE TABLE IF NOT EXISTS %s", table.name, table.name)
		}
	}
	notNull := regexp.MustCompile(`(?i)\bNOT\s+NULL\b`)
	for _, column := range additiveColumns {
		if !strings.HasPrefix(column.ddl, "ALTER TABLE "+column.table+" ADD COLUMN "+column.column+" ") {
			t.Errorf("column %s.%s: ddl must be ALTER TABLE %s ADD COLUMN %s ...", column.table, column.column, column.table, column.column)
		}
		if notNull.MatchString(column.ddl) && !strings.Contains(strings.ToUpper(column.ddl), "DEFAULT") {
			t.Errorf("column %s.%s: a NOT NULL column needs a DEFAULT", column.table, column.column)
		}
	}
	for _, index := range additiveIndexes {
		if !strings.HasPrefix(index.ddl, "CREATE INDEX IF NOT EXISTS "+index.name+" ") && !strings.HasPrefix(index.ddl, "CREATE UNIQUE INDEX IF NOT EXISTS "+index.name+" ") {
			t.Errorf("index %s: ddl must start with CREATE [UNIQUE] INDEX IF NOT EXISTS %s", index.name, index.name)
		}
	}
}

// An older database lacks every additive object. Reopening it must produce
// exactly the layout of a fresh database, keep its data, and keep the schema
// version so older binaries can still open it.
func TestAdditiveSchemaUpgradesOldDatabases(t *testing.T) {
	saved := additiveColumns
	t.Cleanup(func() { additiveColumns = saved })
	additiveColumns = append(append([]additiveColumn(nil), saved...), additiveColumn{
		table: "servers", column: "zz_additive_probe",
		ddl:      "ALTER TABLE servers ADD COLUMN zz_additive_probe TEXT NOT NULL DEFAULT ''",
		backfill: "UPDATE servers SET zz_additive_probe='backfilled'",
	})

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hub.db")
	store, err := OpenStore(ctx, path, StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertServer(ctx, testServer()); err != nil {
		t.Fatal(err)
	}
	want := schemaSnapshot(t, store)
	version := schemaVersion(t, store)

	// Strip the additive layout, as a database from an older release would be.
	for _, index := range additiveIndexes {
		mustExec(t, store, "DROP INDEX "+index.name)
	}
	for _, column := range additiveColumns {
		mustExec(t, store, "ALTER TABLE "+column.table+" DROP COLUMN "+column.column)
	}
	for _, table := range additiveTables {
		mustExec(t, store, "DROP TABLE "+table.name)
	}
	store.Close()

	for reopen := 0; reopen < 2; reopen++ { // the second open must be a no-op
		store, err = OpenStore(ctx, path, StoreOptions{})
		if err != nil {
			t.Fatalf("reopen %d: %v", reopen, err)
		}
		if got := schemaSnapshot(t, store); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("reopen %d: layout differs from a fresh database\n got: %v\nwant: %v", reopen, got, want)
		}
		if got := schemaVersion(t, store); got != version {
			t.Fatalf("additive changes must not bump the schema version: %d -> %d", version, got)
		}
		var probe string
		if err := store.db.QueryRowContext(ctx, `SELECT zz_additive_probe FROM servers`).Scan(&probe); err != nil || probe != "backfilled" {
			t.Fatalf("backfill: %q, %v", probe, err)
		}
		if servers, err := store.ListServers(ctx, 10); err != nil || len(servers) != 1 {
			t.Fatalf("existing data after upgrade: %v, %v", servers, err)
		}
		store.Close()
	}
}

func schemaSnapshot(t *testing.T, store *Store) []string {
	t.Helper()
	rows, err := store.db.Query(`SELECT type||' '||name||' '||COALESCE(sql,'') FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		// ALTER TABLE rewrites the stored CREATE statement's spacing; compare
		// tokens rather than formatting.
		out = append(out, strings.Join(strings.Fields(line), " "))
	}
	sort.Strings(out)
	return out
}

func schemaVersion(t *testing.T, store *Store) int {
	t.Helper()
	var version int
	if err := store.db.QueryRow(`SELECT version FROM schema_meta`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

func mustExec(t *testing.T, store *Store, statement string) {
	t.Helper()
	if _, err := store.db.Exec(statement); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}
