package monitoring

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/dbschema"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const CurrentSchemaVersion = 7

// StoreSchemaRegistry describes the persisted database format owned by this
// binary. Historical checkpoints 1 through 4 used an unversioned, idempotent
// layout reconciler; their numbers do not imply independently defined layouts.
// Version 5 fixes timestamp encoding; version 6 captures the later additions
// that previously ran on every open without advancing the schema version.
// Version 7 adds per-server ingest authority tables used by cutover fences.
func StoreSchemaRegistry() dbschema.SchemaRegistry {
	r := dbschema.SchemaRegistry{MinReadable: 1, Current: CurrentSchemaVersion}
	for from := 1; from < 4; from++ {
		r.Steps = append(r.Steps, dbschema.SchemaStep{From: from, To: from + 1,
			Name: fmt.Sprintf("legacy checkpoint %d layout reconciliation", from), Apply: reconcileStoreSchema})
	}
	r.Steps = append(r.Steps,
		dbschema.SchemaStep{From: 4, To: 5, Name: "canonical persisted timestamps", Destructive: true,
			Apply: func(ctx context.Context, tx *sql.Tx) error {
				if err := reconcileStoreSchema(ctx, tx); err != nil {
					return err
				}
				return migratePersistedTimesTx(ctx, tx)
			}},
		dbschema.SchemaStep{From: 5, To: 6, Name: "versioned durable layout and backfills", Apply: reconcileStoreSchema},
		dbschema.SchemaStep{From: 6, To: 7, Name: "per-server ingest authority fences", Apply: reconcileStoreSchema},
	)
	return r
}

// CheckSchemaCompatibility reads the live metadata without changing it. It is
// useful for local preflight; candidate releases must advertise their own
// compatibility range to prove compatibility with a different binary.
func (s *Store) CheckSchemaCompatibility(ctx context.Context) error {
	version, err := dbschema.ReadSchemaVersion(ctx, s.db)
	if err != nil {
		return err
	}
	return StoreSchemaRegistry().CheckSchemaCompatibility(version)
}

func (s *Store) migrate(ctx context.Context) error {
	var metadata, tables int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_meta'`).Scan(&metadata); err != nil {
		return err
	}
	if metadata == 0 {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		// OpenStore uses BEGIN IMMEDIATE. Recheck after acquiring SQLite's
		// cross-process writer lock: another startup may have initialized the
		// database while this connection was waiting for that lock.
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_meta'`).Scan(&metadata); err != nil {
			return err
		}
		if metadata != 0 {
			if err := tx.Rollback(); err != nil {
				return err
			}
			return s.migrate(ctx)
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE substr(name,1,7) <> 'sqlite_'`).Scan(&tables); err != nil {
			return err
		}
		if tables != 0 {
			return errors.New("database: nonempty store has no schema metadata")
		}
		if _, err := tx.ExecContext(ctx, `CREATE TABLE schema_meta(version INTEGER NOT NULL); INSERT INTO schema_meta VALUES(?); CREATE TABLE schema_initialization_pending(id INTEGER PRIMARY KEY CHECK(id=1)); INSERT INTO schema_initialization_pending VALUES(1)`, CurrentSchemaVersion); err != nil {
			return err
		}
		if err := reconcileStoreSchema(ctx, tx); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}

	} else {
		version, err := dbschema.ReadSchemaVersion(ctx, s.db)
		if err != nil {
			return err
		}
		registry := StoreSchemaRegistry()
		if err := registry.CheckSchemaCompatibility(version); err != nil {
			return err
		}
		// Snapshot before even a non-destructive historical checkpoint. A later
		// timestamp rewrite must be recoverable from the original startup state.
		var snapshot string
		var snapshotDigest [32]byte
		if version < 5 {
			var cleanup func()
			snapshot, cleanup, err = s.verifyMigrationBackup(ctx, version)
			if cleanup != nil {
				defer cleanup()
			}
			if err != nil {
				return fmt.Errorf("pre-migration backup: %w", err)
			}
			snapshotDigest, err = dbschema.SnapshotDigest(snapshot)
			if err != nil {
				return err
			}
		}
		if _, err := s.db.ExecContext(ctx, `PRAGMA foreign_keys=ON;`); err != nil {
			return err
		}
		_, err = registry.RunSchemaMigrations(ctx, s.db, dbschema.SchemaMigrationOptions{
			BackupVerified: func(ctx context.Context, from, to int) error {
				if snapshot == "" {
					return errors.New("database: no verified migration snapshot")
				}
				if err := dbschema.VerifyPrivateSQLiteBackup(ctx, snapshot); err != nil {
					return err
				}
				digest, err := dbschema.SnapshotDigest(snapshot)
				if err != nil {
					return err
				}
				if digest != snapshotDigest {
					return errors.New("database: verified migration snapshot changed")
				}
				return nil
			},
		})
		if err != nil {
			return err
		}
	}
	if err := s.finalizeInitialization(ctx); err != nil {
		return err
	}
	// Connection configuration follows compatibility checks and successful
	// migration. An unsupported database never reaches a mutating pragma.
	if _, err := s.db.ExecContext(ctx, `PRAGMA foreign_keys=ON;`); err != nil {
		return err
	}
	if err := s.enableWAL(ctx); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `PRAGMA synchronous=NORMAL;`)
	return err
}

// The pending table commits with fresh schema creation. It survives cancellation
// or a crash until incremental vacuum is persisted, then disappears. Existing
// databases without the marker retain their historical vacuum configuration.
func (s *Store) finalizeInitialization(ctx context.Context) error {
	return s.retryStartupSQLite(ctx, func(ctx context.Context) error {
		var pending int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_initialization_pending'`).Scan(&pending); err != nil {
			return err
		}
		if pending == 0 {
			return nil
		}
		var mode int
		if err := s.db.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode); err != nil {
			return err
		}
		if mode != 2 {
			// BEGIN IMMEDIATE fixes the file header before schema creation, so
			// enabling incremental vacuum requires building pointer maps afterward.
			if _, err := s.db.ExecContext(ctx, `PRAGMA auto_vacuum=INCREMENTAL; VACUUM;`); err != nil {
				return err
			}
			if err := s.db.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode); err != nil {
				return err
			}
			if mode != 2 {
				return fmt.Errorf("database: initialization vacuum mode is %d, expected 2", mode)
			}
		}
		_, err := s.db.ExecContext(ctx, `DROP TABLE IF EXISTS schema_initialization_pending`)
		return err
	})
}

func (s *Store) enableWAL(ctx context.Context) error {
	want := "wal"
	if s.path == ":memory:" {
		want = "memory"
	}
	return s.retryStartupSQLite(ctx, func(ctx context.Context) error {
		var mode string
		if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode=WAL`).Scan(&mode); err != nil {
			return err
		}
		if mode != want {
			return fmt.Errorf("database: journal mode is %q, expected %q", mode, want)
		}
		return nil
	})
}

func (s *Store) retryStartupSQLite(ctx context.Context, run func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// SQLite's native busy handler can delay context cancellation. Keep
	// each startup attempt short; normal writer waits resume on success.
	if _, err := s.db.ExecContext(ctx, `PRAGMA busy_timeout=100;`); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := run(ctx)
		if err == nil {
			_, err := s.db.ExecContext(ctx, `PRAGMA busy_timeout=5000;`)
			return err
		}
		var sqliteErr *sqlite.Error
		if !errors.As(err, &sqliteErr) || (sqliteErr.Code()&255 != sqlite3.SQLITE_BUSY && sqliteErr.Code()&255 != sqlite3.SQLITE_LOCKED) {
			return err
		}
		// SQLite lock upgrades can return BUSY without using busy_timeout.
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func storeDiskPath(path string) (string, error) {
	if strings.HasPrefix(path, "file:") {
		u, err := url.Parse(path)
		if err != nil || u.Host != "" {
			return "", errors.New("database: invalid SQLite file URI")
		}
		for key := range u.Query() {
			if strings.HasPrefix(key, "_") {
				return "", errors.New("database: connection directives are not permitted in a store path")
			}
		}
		if u.Query().Get("mode") == "memory" || u.Opaque == ":memory:" {
			return "", nil
		}
		if u.Opaque != "" {
			path, err = url.PathUnescape(u.Opaque)
			if err != nil {
				return "", err
			}
		} else {
			path = u.Path
		}
	}
	if path == ":memory:" {
		return "", nil
	}
	return filepath.Abs(path)
}

func (s *Store) verifyMigrationBackup(ctx context.Context, version int) (string, func(), error) {
	path, err := storeDiskPath(s.path)
	if err != nil {
		return "", nil, err
	}
	directory := path + ".schema-backups"
	var cleanup func()
	if path == "" {
		directory, err = os.MkdirTemp("", "payesh-schema-backup-")
		if err != nil {
			return "", nil, err
		}
		cleanup = func() { _ = os.RemoveAll(directory) }
	}
	limit := s.maxBytes
	if limit <= 0 {
		limit = DefaultDatabaseLimit
	}
	snapshot, err := dbschema.CreateMigrationBackup(ctx, s.db, directory, version, limit)
	return snapshot, cleanup, err
}
