package updater

import (
	"context"
	"database/sql"

	"github.com/Real-kia/payesh/internal/dbschema"
)

// SchemaStep is a forward-only transactional migration.
type SchemaStep = dbschema.SchemaStep

// SchemaRegistry declares the exact ordered compatibility path.
type SchemaRegistry = dbschema.SchemaRegistry

// SchemaMigrationOptions supplies the verified pre-migration backup gate.
type SchemaMigrationOptions = dbschema.SchemaMigrationOptions

func ReadSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	return dbschema.ReadSchemaVersion(ctx, db)
}
