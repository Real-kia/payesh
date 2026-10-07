package updater

import (
	"context"
	"database/sql"

	"github.com/Real-kia/payesh/internal/dbschema"
)

// BackupSQLite creates and verifies a consistent immutable SQLite snapshot.
func BackupSQLite(ctx context.Context, source *sql.DB, destination string) error {
	return dbschema.BackupSQLite(ctx, source, destination, false)
}

func VerifySQLiteBackup(ctx context.Context, path string) error {
	return dbschema.VerifySQLiteBackup(ctx, path)
}
