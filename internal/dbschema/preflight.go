package dbschema

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	_ "modernc.org/sqlite"
	"net/url"
	"os"
	"path/filepath"
)

// ReadInstalledSchemaVersion inspects a real installed database without
// creation or migrations. Normal read-only SQLite access includes committed
// WAL data; immutable=1 would incorrectly ignore that data in a live store.
func ReadInstalledSchemaVersion(ctx context.Context, path string) (version int, exists bool, resultErr error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return 0, false, errors.New("database: schema preflight requires a clean absolute file path")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("database: inspect installed database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return 0, true, errors.New("database: installed database must be a regular non-symlink file")
	}
	uri := (&url.URL{Scheme: "file", Path: path}).String() + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return 0, true, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	version, err = ReadSchemaVersion(ctx, db)
	if err != nil {
		return 0, true, err
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		return 0, true, errors.New("database: installed database changed during schema preflight")
	}
	return version, true, nil
}
