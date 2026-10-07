package dbschema

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SchemaStep is one forward-only database migration. Apply must be safe inside
// the transaction supplied by RunSchemaMigrations.
type SchemaStep struct {
	From, To    int
	Name        string
	Destructive bool
	Apply       func(context.Context, *sql.Tx) error
}

// SchemaRegistry declares the schemas a binary can open and the exact ordered
// migration path it owns. MinReadable prevents an old binary from opening a
// database whose representation it cannot safely understand.
type SchemaRegistry struct {
	MinReadable int
	Current     int
	Steps       []SchemaStep
}

type SchemaMigrationOptions struct {
	// BackupVerified must verify a restorable pre-migration backup. It is called
	// before any destructive step and outside the database transaction.
	BackupVerified func(context.Context, int, int) error
}

func (r SchemaRegistry) Validate() error {
	if r.MinReadable < 1 || r.Current < r.MinReadable {
		return errors.New("database: invalid schema compatibility range")
	}
	seen := make(map[int]bool, len(r.Steps))
	for _, step := range r.Steps {
		if step.From < r.MinReadable || step.From >= r.Current || step.To != step.From+1 || step.Name == "" || step.Apply == nil || seen[step.From] {
			return errors.New("database: invalid schema migration registry")
		}
		seen[step.From] = true
	}
	for version := r.MinReadable; version < r.Current; version++ {
		if !seen[version] {
			return fmt.Errorf("database: schema migration path has a gap at version %d", version)
		}
	}
	return nil
}

// CheckSchemaCompatibility is side-effect free and should be used during
// update/role preflight. Newer and too-old databases fail closed.
func (r SchemaRegistry) CheckSchemaCompatibility(version int) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if version < r.MinReadable {
		return fmt.Errorf("database: database schema %d is older than minimum readable schema %d", version, r.MinReadable)
	}
	if version > r.Current {
		return fmt.Errorf("database: database schema %d is newer than supported schema %d", version, r.Current)
	}
	return nil
}

func ReadSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	if db == nil {
		return 0, errors.New("database: database is required")
	}
	return readSchemaVersion(ctx, db)
}

// ReadSchemaVersionTx validates metadata in the caller's consistent snapshot.
func ReadSchemaVersionTx(ctx context.Context, tx *sql.Tx) (int, error) {
	if tx == nil {
		return 0, errors.New("database: transaction is required")
	}
	return readSchemaVersion(ctx, tx)
}

func readSchemaVersion(ctx context.Context, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (int, error) {
	var count, minimum, maximum, invalid int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MIN(version),0),COALESCE(MAX(version),0),COALESCE(SUM(typeof(version) <> 'integer'),0) FROM schema_meta`).Scan(&count, &minimum, &maximum, &invalid); err != nil {
		return 0, fmt.Errorf("database: read schema version: %w", err)
	}
	if count != 1 || minimum != maximum || minimum < 1 || invalid != 0 {
		return 0, errors.New("database: invalid persisted schema version")
	}
	return minimum, nil
}

// RunSchemaMigrations applies one exact, transactional, forward-only path.
// The schema version is advanced in the same transaction as its data changes.
func (r SchemaRegistry) RunSchemaMigrations(ctx context.Context, db *sql.DB, opts SchemaMigrationOptions) (int, error) {
	version, err := ReadSchemaVersion(ctx, db)
	if err != nil {
		return 0, err
	}
	if err := r.CheckSchemaCompatibility(version); err != nil {
		return version, err
	}
	steps := make(map[int]SchemaStep, len(r.Steps))
	for _, step := range r.Steps {
		steps[step.From] = step
	}
	for version < r.Current {
		step := steps[version]
		if step.Destructive {
			if opts.BackupVerified == nil {
				return version, errors.New("database: destructive schema migration requires a verified backup")
			}
			if err := opts.BackupVerified(ctx, step.From, step.To); err != nil {
				return version, fmt.Errorf("database: verify pre-migration backup: %w", err)
			}
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return version, fmt.Errorf("database: begin schema migration: %w", err)
		}
		// Startup connections use BEGIN IMMEDIATE, so the transaction owns
		// SQLite's writer lock before this read. A competing process may have
		// advanced the schema since our preflight or backup verification.
		lockedVersion, err := readSchemaVersion(ctx, tx)
		if err == nil {
			err = r.CheckSchemaCompatibility(lockedVersion)
		}
		if err != nil {
			tx.Rollback()
			return version, err
		}
		if lockedVersion != version {
			if err := tx.Rollback(); err != nil {
				return version, err
			}
			if lockedVersion < version {
				return version, errors.New("database: schema version decreased concurrently")
			}
			version = lockedVersion
			continue
		}
		if err := step.Apply(ctx, tx); err != nil {
			tx.Rollback()
			return version, fmt.Errorf("database: apply schema migration %s: %w", step.Name, err)
		}
		result, err := tx.ExecContext(ctx, `UPDATE schema_meta SET version=? WHERE version=?`, step.To, step.From)
		if err == nil {
			var changed int64
			changed, err = result.RowsAffected()
			if err == nil && changed != 1 {
				err = errors.New("schema version changed concurrently")
			}
		}
		if err != nil {
			tx.Rollback()
			return version, fmt.Errorf("database: record schema migration: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return version, fmt.Errorf("database: commit schema migration: %w", err)
		}
		version = step.To
	}
	return version, nil
}
