package monitoring

import (
	"context"
	"fmt"
)

// Additive schema changes are applied on every open, whatever version the
// database is at. Use them for anything an older binary can safely ignore:
// a new table, a new nullable or defaulted column, or a new index. They do
// not bump CurrentSchemaVersion, so an older release can still open the
// database and a rollback keeps working.
//
// Anything that rewrites, renames, drops, or reinterprets existing data still
// needs a versioned SchemaStep (with its pre-migration backup).
//
// To add a change, append to the lists below. TestAdditiveSchemaUpgradesOldDatabases
// covers every entry automatically.

type additiveTable struct {
	name string
	ddl  string // must be CREATE TABLE IF NOT EXISTS <name> (...)
}

type additiveColumn struct {
	table  string
	column string
	ddl    string // must be ALTER TABLE <table> ADD COLUMN <column> ..., nullable or with a DEFAULT
	// backfill optionally fills the new column for existing rows. It runs
	// only when the column was just added.
	backfill string
}

type additiveIndex struct {
	name string
	ddl  string // must be CREATE INDEX IF NOT EXISTS <name> ...
}

var additiveTables = []additiveTable{
	// Best-effort country lookups for display. Kept apart from servers so a
	// failed or disabled lookup never touches identity rows or their revision.
	{name: "server_locations", ddl: `CREATE TABLE IF NOT EXISTS server_locations (
  server_id TEXT PRIMARY KEY,
  country_code TEXT NOT NULL DEFAULT '',
  lookup_ip TEXT NOT NULL DEFAULT '',
  peer_ip TEXT NOT NULL DEFAULT '',
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
)`},
	// Nodes acting as a CONNECT relay for servers the hub cannot reach.
	{name: "relay_nodes", ddl: `CREATE TABLE IF NOT EXISTS relay_nodes (
  server_id TEXT PRIMARY KEY,
  port INTEGER NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY (server_id) REFERENCES servers(id) ON DELETE CASCADE
)`},
	// Relay credentials. Only the password hash is stored; the plaintext lives
	// in the target node's configuration (node grants) or in memory for the
	// length of one installation (install grants).
	{name: "relay_grants", ddl: `CREATE TABLE IF NOT EXISTS relay_grants (
  relay_server_id TEXT NOT NULL,
  username TEXT NOT NULL,
  password_sha256 TEXT NOT NULL,
  destinations_json TEXT NOT NULL,
  expires_at TEXT NOT NULL DEFAULT '',
  target_server_id TEXT,
  created_at TEXT NOT NULL,
  PRIMARY KEY (relay_server_id, username),
  FOREIGN KEY (relay_server_id) REFERENCES relay_nodes(server_id) ON DELETE CASCADE,
  FOREIGN KEY (target_server_id) REFERENCES servers(id) ON DELETE CASCADE
)`},
}

var additiveColumns = []additiveColumn{}

var additiveIndexes = []additiveIndex{}

// applyAdditiveSchema brings any database up to the additive layout in one
// transaction. It only inspects sqlite_master and table_info when nothing is
// missing, so it is cheap on every start.
func (s *Store) applyAdditiveSchema(ctx context.Context) error {
	return s.retryStartupSQLite(ctx, func(ctx context.Context) error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, table := range additiveTables {
			if _, err := tx.ExecContext(ctx, table.ddl); err != nil {
				return fmt.Errorf("create table %s: %w", table.name, err)
			}
		}
		for _, column := range additiveColumns {
			var present int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, column.table, column.column).Scan(&present); err != nil {
				return fmt.Errorf("inspect %s.%s: %w", column.table, column.column, err)
			}
			if present > 0 {
				continue
			}
			if _, err := tx.ExecContext(ctx, column.ddl); err != nil {
				return fmt.Errorf("add column %s.%s: %w", column.table, column.column, err)
			}
			if column.backfill != "" {
				if _, err := tx.ExecContext(ctx, column.backfill); err != nil {
					return fmt.Errorf("backfill %s.%s: %w", column.table, column.column, err)
				}
			}
		}
		for _, index := range additiveIndexes {
			if _, err := tx.ExecContext(ctx, index.ddl); err != nil {
				return fmt.Errorf("create index %s: %w", index.name, err)
			}
		}
		return tx.Commit()
	})
}
