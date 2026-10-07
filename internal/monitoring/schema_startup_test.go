package monitoring

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/dbschema"
	"modernc.org/sqlite"
)

func TestOpenStoreRejectsInvalidSchemaWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name string
		ddl  string
	}{
		{"newer", `CREATE TABLE schema_meta(version INTEGER NOT NULL); INSERT INTO schema_meta VALUES(999);`},
		{"negative", `CREATE TABLE schema_meta(version INTEGER NOT NULL); INSERT INTO schema_meta VALUES(-1);`},
		{"fractional", `CREATE TABLE schema_meta(version INTEGER NOT NULL); INSERT INTO schema_meta VALUES(4.5);`},
		{"text", `CREATE TABLE schema_meta(version INTEGER NOT NULL); INSERT INTO schema_meta VALUES('bad');`},
		{"view_without_metadata", `CREATE VIEW evidence AS SELECT 1;`},
		{"zero", `CREATE TABLE schema_meta(version INTEGER NOT NULL); INSERT INTO schema_meta VALUES(0);`},
		{"duplicate", `CREATE TABLE schema_meta(version INTEGER NOT NULL); INSERT INTO schema_meta VALUES(5),(5);`},
		{"empty_metadata", `CREATE TABLE schema_meta(version INTEGER NOT NULL);`},
		{"sqlite_name_prefix_without_metadata", `CREATE TABLE sqliteXdurable(value TEXT); INSERT INTO sqliteXdurable VALUES('keep');`},
		{"missing_metadata", `CREATE TABLE durable_evidence(value TEXT); INSERT INTO durable_evidence VALUES('keep');`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store.sqlite")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(tc.ddl); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			store, openErr := OpenStore(t.Context(), path, StoreOptions{})
			if store != nil {
				_ = store.Close()
			}
			if openErr == nil {
				t.Error("startup accepted unsupported or invalid schema")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Error("rejected startup changed database bytes")
			}
		})
	}
}

func TestSchemaRecoverySnapshotCountsTowardActualStoragePressure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "store.sqlite")
	uri := (&url.URL{Scheme: "file", Path: path}).String()
	backupDir := path + ".schema-backups"
	// Overlapping roots must not charge the same live or recovery file twice.
	options := StoreOptions{MaxBytes: DefaultDatabaseLimit, ManagedPaths: []string{root, backupDir, backupDir + "/."}}
	store, err := OpenStore(t.Context(), uri, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TABLE recovery_evidence(value BLOB); INSERT INTO recovery_evidence VALUES(zeroblob(1048576)); UPDATE schema_meta SET version=4`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(t.Context(), uri, options)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	entries, err := os.ReadDir(backupDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	snapshotBytes := info.Size()
	databaseBytes, err := store.DatabaseBytes()
	if err != nil {
		t.Fatal(err)
	}
	usage, err := store.StorageBytes()
	if err != nil {
		t.Fatal(err)
	}
	if usage != databaseBytes+snapshotBytes {
		t.Fatalf("managed usage=%d live=%d recovery=%d", usage, databaseBytes, snapshotBytes)
	}
	configuredUsage, err := store.budgetUsage(true)
	if err != nil || configuredUsage != usage {
		t.Fatalf("configured usage=%d managed=%d err=%v", configuredUsage, usage, err)
	}
	// Use the existing tiny-budget fixture path to exercise real write admission
	// without generating hundreds of megabytes solely for a pressure test.
	limit := databaseBytes + snapshotBytes/2
	if _, err := store.db.Exec(`UPDATE storage_settings SET max_bytes=? WHERE singleton=1`, limit); err != nil {
		t.Fatal(err)
	}
	if err := store.ensureDerivedWritable(t.Context()); !errors.Is(err, ErrStoragePressure) {
		t.Fatalf("derived admission ignored recovery footprint: %v", err)
	}
	if err := store.ensureWritable(t.Context()); !errors.Is(err, ErrStoragePressure) {
		t.Fatalf("raw admission ignored recovery footprint: %v", err)
	}
	settings, configured, err := store.StorageSettings(t.Context())
	if err != nil || !configured || settings.PressureState != "saving" {
		t.Fatalf("settings=%+v configured=%v err=%v", settings, configured, err)
	}
	status, err := store.StorageStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if status.DatabaseBytes >= limit {
		t.Fatalf("fixture's live DB alone already exceeded budget: live=%d budget=%d", status.DatabaseBytes, limit)
	}
	if status.RecoverySnapshotBytes != snapshotBytes {
		t.Fatalf("status recovery bytes=%d actual=%d", status.RecoverySnapshotBytes, snapshotBytes)
	}
}

func TestOpenStoreRejectsCallerPragmasBeforeOpeningDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_meta(version INTEGER NOT NULL); INSERT INTO schema_meta VALUES(999)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	u := &url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("_pragma", "journal_mode(WAL)")
	u.RawQuery = q.Encode()
	store, err := OpenStore(t.Context(), u.String(), StoreOptions{})
	if store != nil {
		_ = store.Close()
	}
	if err == nil {
		t.Fatal("caller pragma was accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("caller pragma changed rejected database")
	}
}

func TestOpenStoreMigrationBackupFailurePrecedesAllMutations(t *testing.T) {
	for _, gate := range []string{"budget", "symlink", "private-directory", "snapshot-count"} {
		t.Run(gate, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store.sqlite")
			store, err := OpenStore(t.Context(), path, StoreOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(`UPDATE schema_meta SET version=4`); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			options := StoreOptions{}
			dir := path + ".schema-backups"
			switch gate {
			case "budget":
				options.MaxBytes = 1
			case "symlink":
				if err := os.Symlink(t.TempDir(), dir); err != nil {
					t.Fatal(err)
				}
			case "private-directory":
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			case "snapshot-count":
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"first.sqlite", "second.sqlite"} {
					if err := os.WriteFile(filepath.Join(dir, name), []byte("keep"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			store, openErr := OpenStore(t.Context(), path, options)
			if store != nil {
				_ = store.Close()
			}
			if openErr == nil {
				t.Fatal("migration ignored unavailable verified backup")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("backup failure mutated source database")
			}
		})
	}
}

func TestOpenStoreDiskMigrationKeepsRestorablePreRewriteBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store?# with spaces.sqlite")
	uri := (&url.URL{Scheme: "file", Path: path}).String()
	store, err := OpenStore(t.Context(), uri, StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	const legacy = "2026-01-02T03:04:05+03:30"
	if _, err := store.db.Exec(`UPDATE schema_meta SET version=4; INSERT INTO maintenance_windows(id,idempotency_key,starts_at,ends_at,server_ids_json,reason) VALUES('window','key',?,?,'[]','evidence')`, legacy, legacy); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(t.Context(), uri, StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CheckSchemaCompatibility(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(path + ".schema-backups")
	if err != nil || len(entries) != 1 {
		t.Fatalf("snapshot entries=%v err=%v", entries, err)
	}
	backupPath := filepath.Join(path+".schema-backups", entries[0].Name())
	info, err := os.Stat(backupPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup permissions info=%v err=%v", info, err)
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: backupPath}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	var timestamp string
	if err := db.QueryRow(`SELECT version FROM schema_meta`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT starts_at FROM maintenance_windows WHERE id='window'`).Scan(&timestamp); err != nil {
		t.Fatal(err)
	}
	if version != 4 || timestamp != legacy {
		t.Fatalf("snapshot lost original state: version=%d timestamp=%q", version, timestamp)
	}
	// A normal current-version reopen creates no additional snapshot.
	store, err = OpenStore(t.Context(), uri, StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err = os.ReadDir(path + ".schema-backups")
	if err != nil || len(entries) != 1 {
		t.Fatalf("normal reopen created snapshot: entries=%v err=%v", entries, err)
	}
}

func TestOpenStoreVersionFiveLayoutAndVersionAreAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_meta(version INTEGER NOT NULL); INSERT INTO schema_meta VALUES(5);
		CREATE TABLE evidence(value TEXT); INSERT INTO evidence VALUES('durable');
		CREATE TRIGGER refuse_layout_version BEFORE UPDATE ON schema_meta WHEN NEW.version=6 BEGIN SELECT RAISE(ABORT,'injected version failure'); END;`); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(t.Context(), path, StoreOptions{})
	if store != nil {
		_ = store.Close()
	}
	if err == nil {
		t.Fatal("migration succeeded despite version failure")
	}
	var tables int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='jobs'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatal("version5→6 schema changes survived failed version write")
	}
	if _, err := db.Exec(`DROP TRIGGER refuse_layout_version`); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(t.Context(), path, StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var version int
	var evidence string
	if err := store.db.QueryRow(`SELECT version FROM schema_meta`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT value FROM evidence`).Scan(&evidence); err != nil {
		t.Fatal(err)
	}
	if version != CurrentSchemaVersion || evidence != "durable" {
		t.Fatalf("version=%d evidence=%q", version, evidence)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".schema-backups"); !os.IsNotExist(err) {
		t.Fatalf("non-destructive version5 migration created backup: %v", err)
	}
}

func TestOpenStoreTimestampMigrationVersionFailureRollsBackData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	store, err := OpenStore(t.Context(), path, StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const legacy = "2026-01-02T03:04:05+03:30"
	if _, err := db.Exec(`UPDATE schema_meta SET version=4;
		INSERT INTO maintenance_windows(id,idempotency_key,starts_at,ends_at,server_ids_json,reason) VALUES('migration-window','migration-key',?,?,'[]','keep');
		CREATE TRIGGER refuse_schema_version BEFORE UPDATE ON schema_meta WHEN NEW.version>=5 BEGIN SELECT RAISE(ABORT,'injected version write failure'); END;`, legacy, legacy); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(t.Context(), path, StoreOptions{})
	if store != nil {
		_ = store.Close()
	}
	if err == nil {
		t.Fatal("startup succeeded despite version write failure")
	}
	var version int
	var timestamp string
	if err := db.QueryRow(`SELECT version FROM schema_meta`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT starts_at FROM maintenance_windows WHERE id='migration-window'`).Scan(&timestamp); err != nil {
		t.Fatal(err)
	}
	if version != 4 || timestamp != legacy {
		t.Fatalf("partial migration survived failure: version=%d timestamp=%q", version, timestamp)
	}
}

// startupBarrierConnector keeps real, independent SQLite connections but pauses
// the contender at its first write boundary. The winner can then commit between
// the contender's initial metadata read and its initialization transaction.
type startupBarrierConnector struct {
	dsn              string
	reached, release chan struct{}
	once             sync.Once
}

func (c *startupBarrierConnector) Driver() driver.Driver { return &sqlite.Driver{} }
func (c *startupBarrierConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Driver().Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &startupBarrierConn{Conn: conn, barrier: c}, nil
}

type startupBarrierConn struct {
	driver.Conn
	barrier *startupBarrierConnector
}

func (c *startupBarrierConn) pause(ctx context.Context) error {
	var err error
	c.barrier.once.Do(func() {
		close(c.barrier.reached)
		select {
		case <-c.barrier.release:
		case <-ctx.Done():
			err = ctx.Err()
		}
	})
	return err
}
func (c *startupBarrierConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if err := c.pause(ctx); err != nil {
		return nil, err
	}
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}
func (c *startupBarrierConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if strings.Contains(query, "auto_vacuum") {
		if err := c.pause(ctx); err != nil {
			return nil, err
		}
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}
func (c *startupBarrierConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func TestStoreConcurrentFreshInitializationRechecksUnderWriteLock(t *testing.T) {
	for _, version := range []int{CurrentSchemaVersion, 999} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			path := filepath.Join(t.TempDir(), "store.sqlite")
			dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=busy_timeout(5000)&_txlock=immediate"}).String()
			winner, err := sql.Open("sqlite", dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer winner.Close()
			tx, err := winner.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			barrier := &startupBarrierConnector{dsn: dsn, reached: make(chan struct{}), release: make(chan struct{})}
			contender := sql.OpenDB(barrier)
			contender.SetMaxOpenConns(1)
			defer contender.Close()
			result := make(chan error, 1)
			go func() { result <- (&Store{db: contender, path: path}).migrate(ctx) }()
			select {
			case <-barrier.reached:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if _, err := tx.ExecContext(ctx, `CREATE TABLE schema_meta(version INTEGER NOT NULL); INSERT INTO schema_meta VALUES(?)`, version); err != nil {
				t.Fatal(err)
			}
			if version == CurrentSchemaVersion {
				if err := reconcileStoreSchema(ctx, tx); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			close(barrier.release)
			err = <-result
			if version == CurrentSchemaVersion && err != nil {
				t.Fatalf("concurrent initializer did not converge: %v", err)
			}
			if version > CurrentSchemaVersion {
				if err == nil || !strings.Contains(err.Error(), "newer than supported") {
					t.Fatalf("expected compatibility refusal, got %v", err)
				}
				after, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, after) {
					t.Fatal("rejected contender mutated the competing newer database")
				}
			}
			var count, got int
			if err := winner.QueryRowContext(ctx, `SELECT COUNT(*),MIN(version) FROM schema_meta`).Scan(&count, &got); err != nil {
				t.Fatal(err)
			}
			if count != 1 || got != version {
				t.Fatalf("schema metadata count=%d version=%d", count, got)
			}
		})
	}
}

func TestStoreConcurrentHistoricalMigrationRechecksUnderWriteLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "store.sqlite")
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=busy_timeout(5000)&_txlock=immediate"}).String()
	winner, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer winner.Close()
	if _, err := winner.ExecContext(ctx, `CREATE TABLE schema_meta(version INTEGER NOT NULL); INSERT INTO schema_meta VALUES(1); CREATE TABLE migration_evidence(value TEXT);`); err != nil {
		t.Fatal(err)
	}
	registry := dbschema.SchemaRegistry{MinReadable: 1, Current: 2, Steps: []dbschema.SchemaStep{{From: 1, To: 2, Name: "once", Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO migration_evidence VALUES('applied')`)
		return err
	}}}}
	barrier := &startupBarrierConnector{dsn: dsn, reached: make(chan struct{}), release: make(chan struct{})}
	contender := sql.OpenDB(barrier)
	contender.SetMaxOpenConns(1)
	defer contender.Close()
	result := make(chan error, 1)
	go func() {
		version, err := registry.RunSchemaMigrations(ctx, contender, dbschema.SchemaMigrationOptions{})
		if err == nil && version != 2 {
			err = fmt.Errorf("schema version=%d", version)
		}
		result <- err
	}()
	select {
	case <-barrier.reached:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := registry.RunSchemaMigrations(ctx, winner, dbschema.SchemaMigrationOptions{}); err != nil {
		t.Fatal(err)
	}
	close(barrier.release)
	if err := <-result; err != nil {
		t.Fatalf("concurrent migration did not converge: %v", err)
	}
	var applied int
	if err := winner.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_evidence`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != 1 {
		t.Fatalf("migration applied %d times", applied)
	}
}

func TestOpenStoreFreshInitializationKeepsStoragePragmas(t *testing.T) {
	store, err := OpenStore(t.Context(), filepath.Join(t.TempDir(), "store.sqlite"), StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, pragma := range []struct {
		name string
		want int
	}{{"auto_vacuum", 2}, {"foreign_keys", 1}, {"busy_timeout", 5000}} {
		var got int
		if err := store.db.QueryRowContext(t.Context(), "PRAGMA "+pragma.name).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != pragma.want {
			t.Fatalf("%s=%d, want %d", pragma.name, got, pragma.want)
		}
	}
}

func TestOpenStoreConcurrentFreshStartsRetainCommittedSamples(t *testing.T) {
	assertConcurrentStoreStartsRetainCommittedSamples(t, filepath.Join(t.TempDir(), "store.sqlite"))
}

func TestOpenStoreConcurrentPendingInitializationRetainsCommittedSamples(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	prepareInterruptedFreshStore(t, path)
	assertConcurrentStoreStartsRetainCommittedSamples(t, path)
}

func assertConcurrentStoreStartsRetainCommittedSamples(t *testing.T, path string) {
	t.Helper()
	const workers = 8
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, workers)
	for worker := range workers {
		go func() {
			<-start
			store, err := OpenStore(ctx, path, StoreOptions{})
			if err != nil {
				results <- fmt.Errorf("worker %d open: %w", worker, err)
				return
			}
			defer store.Close()
			server := testServer()
			server.ID = contracts.ServerID(fmt.Sprintf("server-local-%04d", worker))
			if err := store.EnsureServer(ctx, server); err != nil {
				results <- fmt.Errorf("worker %d ensure: %w", worker, err)
				return
			}
			at := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
			sample := contracts.MetricSample{ServerID: server.ID, CollectorEpoch: "concurrent-startup-epoch", Sequence: 0, ObservedAt: at, ReceivedAt: at, Values: map[string]float64{"cpu.utilization": float64(worker)}}
			_, err = store.IngestSamples(ctx, server.ID, []contracts.MetricSample{sample}, nil)
			if err != nil {
				err = fmt.Errorf("worker %d ingest: %w", worker, err)
			}
			results <- err
		}()
	}
	close(start)
	for range workers {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	store, err := OpenStore(ctx, path, StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	at := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	for worker := range workers {
		serverID := contracts.ServerID(fmt.Sprintf("server-local-%04d", worker))
		samples, _, err := store.QueryMetricSamplesRange(ctx, serverID, at, at, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(samples) != 1 || samples[0].Values["cpu.utilization"] != float64(worker) {
			t.Errorf("worker %d committed sample lost or changed: %+v", worker, samples)
		}
	}
	var vacuum int
	if err := store.db.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&vacuum); err != nil {
		t.Fatal(err)
	}
	if vacuum != 2 {
		t.Fatalf("auto_vacuum=%d, want 2", vacuum)
	}
	var pending int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_initialization_pending'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatal("successful initialization left its pending marker")
	}
}

func TestStoreWALContentionHonorsContext(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprintf("deadline=%t", deadline), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store.sqlite")
			dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=busy_timeout(5000)&_txlock=exclusive"}).String()
			holder, err := sql.Open("sqlite", dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Close()
			if _, err := holder.ExecContext(t.Context(), `CREATE TABLE evidence(value TEXT)`); err != nil {
				t.Fatal(err)
			}
			tx, err := holder.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			contender, err := sql.Open("sqlite", dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer contender.Close()
			ctx, cancel := context.WithCancel(t.Context())
			want := error(context.Canceled)
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), 50*time.Millisecond)
				want = context.DeadlineExceeded
			} else {
				timer := time.AfterFunc(50*time.Millisecond, cancel)
				defer timer.Stop()
			}
			defer cancel()
			started := time.Now()
			err = (&Store{db: contender, path: path}).enableWAL(ctx)
			if !errors.Is(err, want) {
				t.Fatalf("expected %v during WAL contention, got %v", want, err)
			}
			if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
				t.Fatalf("WAL retry did not honor context promptly: %s", elapsed)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if err := (&Store{db: contender, path: path}).enableWAL(t.Context()); err != nil {
				t.Fatalf("WAL setup failed after releasing contention: %v", err)
			}
		})
	}
}

type vacuumCancelConnector struct {
	dsn      string
	cancelOn string
	cancel   context.CancelFunc
}

func (c *vacuumCancelConnector) Driver() driver.Driver { return &sqlite.Driver{} }
func (c *vacuumCancelConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Driver().Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &vacuumCancelConn{Conn: conn, cancel: c.cancel, cancelOn: c.cancelOn}, nil
}

type vacuumCancelConn struct {
	driver.Conn
	cancelOn string
	cancel   context.CancelFunc
}

func (c *vacuumCancelConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}
func (c *vacuumCancelConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
}
func (c *vacuumCancelConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	selector := c.cancelOn
	if selector == "" {
		selector = "VACUUM"
	}
	if strings.Contains(q, selector) {
		c.cancel()
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
}
func prepareInterruptedFreshStore(t *testing.T, path string) {
	t.Helper()
	prepareFreshStoreInterruptedAt(t, path, "VACUUM")
}

func prepareFreshStoreInterruptedAt(t *testing.T, path, cancelOn string) {
	t.Helper()
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=busy_timeout(10000)&_txlock=immediate"}).String()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	db := sql.OpenDB(&vacuumCancelConnector{dsn: dsn, cancel: cancel, cancelOn: cancelOn})
	db.SetMaxOpenConns(1)
	err := (&Store{db: db, path: path}).migrate(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled fresh initialization, got %v", err)
	}
	var pending int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_initialization_pending'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatal("interrupted fresh initialization lost its durable marker")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenStoreInterruptedFreshVacuumRecoversConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	prepareInterruptedFreshStore(t, path)
	store, err := OpenStore(t.Context(), path, StoreOptions{})
	if err != nil {
		t.Fatalf("restart failed: %v", err)
	}
	defer store.Close()
	var version, vacuum int
	if err := store.db.QueryRow(`SELECT version FROM schema_meta`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&vacuum); err != nil {
		t.Fatal(err)
	}
	t.Logf("restart accepted schema=%d auto_vacuum=%d", version, vacuum)
	if vacuum != 2 {
		t.Fatalf("restart permanently skips interrupted incremental-vacuum setup: mode=%d", vacuum)
	}
}

func TestOpenStoreLegacyCurrentSchemaWithoutPendingInitializationKeepsVacuumMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "_txlock=immediate"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`CREATE TABLE schema_meta(version INTEGER NOT NULL); INSERT INTO schema_meta VALUES(?)`, CurrentSchemaVersion); err != nil {
		t.Fatal(err)
	}
	if err := reconcileStoreSchema(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(t.Context(), path, StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var mode int
	if err := store.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != 0 {
		t.Fatalf("ordinary current schema was unexpectedly vacuumed: mode=%d", mode)
	}
}

func TestOpenStorePendingMarkerAfterVacuumDoesNotRepeatRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	prepareFreshStoreInterruptedAt(t, path, "DROP TABLE")
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=busy_timeout(10000)&_txlock=immediate"}).String()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Any repeated full vacuum would cancel this real connection and fail startup.
	db := sql.OpenDB(&vacuumCancelConnector{dsn: dsn, cancel: cancel})
	db.SetMaxOpenConns(1)
	defer db.Close()
	if err := (&Store{db: db, path: path}).migrate(ctx); err != nil {
		t.Fatalf("restart repeated completed vacuum or failed finalization: %v", err)
	}
	var pending, mode int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_initialization_pending'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if pending != 0 || mode != 2 {
		t.Fatalf("restart pending=%d auto_vacuum=%d", pending, mode)
	}
}
