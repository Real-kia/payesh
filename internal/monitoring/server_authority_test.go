package monitoring

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

func authorityDigest(seed string) string {
	return strings.Repeat(seed, 64)[:64]
}

func authoritySample(server contracts.ServerID, sequence uint64) contracts.MetricSample {
	at := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(sequence) * time.Second)
	return contracts.MetricSample{
		ServerID: server, CollectorEpoch: "authority-epoch-0001", Sequence: sequence,
		ObservedAt: at, ReceivedAt: at, Values: map[string]float64{"cpu.utilization": float64(sequence % 100)},
	}
}

func openAuthorityStore(t *testing.T) (*Store, contracts.Server) {
	t.Helper()
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	server := testServer()
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	return store, server
}

func TestServerAuthorityInitializationIsExplicitAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store, server := openAuthorityStore(t)
	if _, found, err := store.GetServerAuthority(ctx, server.ID); err != nil || found {
		t.Fatalf("unmanaged server must have no authority row: found=%v err=%v", found, err)
	}
	first, err := store.InitializeServerAuthority(ctx, server.ID, "owner-hub-aaaa")
	if err != nil {
		t.Fatal(err)
	}
	if first.Generation != 1 || first.State != AuthorityActive || first.Owner != "owner-hub-aaaa" {
		t.Fatalf("initial authority=%+v", first)
	}
	again, err := store.InitializeServerAuthority(ctx, server.ID, "owner-hub-aaaa")
	if err != nil || again != first {
		t.Fatalf("idempotent initialize=%+v err=%v want %+v", again, err, first)
	}
	if _, err := store.InitializeServerAuthority(ctx, server.ID, "owner-hub-bbbb"); !errors.Is(err, ErrAuthorityConflict) {
		t.Fatalf("a different owner must not take over an initialized server: %v", err)
	}
	if _, err := store.InitializeServerAuthority(ctx, "server-missing-0001", "owner-hub-aaaa"); err == nil {
		t.Fatal("authority was initialized for an unregistered server")
	}
	if _, err := store.InitializeServerAuthority(ctx, server.ID, "bad owner!"); err == nil {
		t.Fatal("unsafe owner identifier accepted")
	}
}

func TestUnownedServerKeepsLegacyIngest(t *testing.T) {
	ctx := context.Background()
	store, server := openAuthorityStore(t)
	if _, err := store.IngestSamples(ctx, server.ID, []contracts.MetricSample{authoritySample(server.ID, 0)}, nil); err != nil {
		t.Fatalf("a server without an authority row must keep legacy ingest: %v", err)
	}
}

func TestFrozenServerRejectsIngestWithoutAcknowledgement(t *testing.T) {
	ctx := context.Background()
	store, server := openAuthorityStore(t)
	if _, err := store.InitializeServerAuthority(ctx, server.ID, "owner-hub-aaaa"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.IngestSamples(ctx, server.ID, []contracts.MetricSample{authoritySample(server.ID, 0)}, nil); err != nil {
		t.Fatalf("active owner ingest: %v", err)
	}
	frozen, err := store.FreezeServerAuthority(ctx, AuthorityTransition{ServerID: server.ID, CutoverID: "cutover-0001", RequestDigest: authorityDigest("a"), FrontierDigest: authorityDigest("b")})
	if err != nil {
		t.Fatal(err)
	}
	if frozen.State != AuthorityFrozen || frozen.Generation != 2 || frozen.CutoverID != "cutover-0001" {
		t.Fatalf("frozen authority=%+v", frozen)
	}
	result, err := store.IngestSamples(ctx, server.ID, []contracts.MetricSample{authoritySample(server.ID, 1)}, nil)
	if !errors.Is(err, ErrServerNotAuthoritative) || result.Inserted != 0 {
		t.Fatalf("frozen ingest result=%+v err=%v", result, err)
	}
	var rows int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM metric_samples WHERE server_id=?`, string(server.ID)).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("frozen ingest changed durable rows: rows=%d err=%v", rows, err)
	}
}

func TestAuthorityTransitionsAreMonotonicIdempotentAndBound(t *testing.T) {
	ctx := context.Background()
	store, server := openAuthorityStore(t)
	if _, err := store.FreezeServerAuthority(ctx, AuthorityTransition{ServerID: server.ID, CutoverID: "cutover-0001", RequestDigest: authorityDigest("a"), FrontierDigest: authorityDigest("b")}); !errors.Is(err, ErrAuthorityConflict) {
		t.Fatalf("freezing an uninitialized server must be refused: %v", err)
	}
	if _, err := store.InitializeServerAuthority(ctx, server.ID, "owner-hub-aaaa"); err != nil {
		t.Fatal(err)
	}
	freeze := AuthorityTransition{ServerID: server.ID, CutoverID: "cutover-0001", RequestDigest: authorityDigest("a"), FrontierDigest: authorityDigest("b")}
	first, err := store.FreezeServerAuthority(ctx, freeze)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := store.FreezeServerAuthority(ctx, freeze)
	if err != nil || replay != first {
		t.Fatalf("exact replay=%+v err=%v want %+v", replay, err, first)
	}
	changed := freeze
	changed.RequestDigest = authorityDigest("c")
	if _, err := store.FreezeServerAuthority(ctx, changed); !errors.Is(err, ErrAuthorityConflict) {
		t.Fatalf("same cutover with different request must conflict: %v", err)
	}
	other := freeze
	other.CutoverID = "cutover-0002"
	if _, err := store.FreezeServerAuthority(ctx, other); !errors.Is(err, ErrAuthorityConflict) {
		t.Fatalf("a different cutover must not freeze an already frozen server: %v", err)
	}
	wrong := freeze
	wrong.CutoverID = "cutover-0002"
	if _, err := store.RelinquishServerAuthority(ctx, wrong); !errors.Is(err, ErrAuthorityConflict) {
		t.Fatalf("relinquish for another cutover must be refused: %v", err)
	}
	done, err := store.RelinquishServerAuthority(ctx, freeze)
	if err != nil {
		t.Fatal(err)
	}
	if done.State != AuthorityRelinquished || done.Generation != 3 {
		t.Fatalf("relinquished authority=%+v", done)
	}
	again, err := store.RelinquishServerAuthority(ctx, freeze)
	if err != nil || again != done {
		t.Fatalf("relinquish replay=%+v err=%v want %+v", again, err, done)
	}
	if _, err := store.FreezeServerAuthority(ctx, other); !errors.Is(err, ErrAuthorityConflict) {
		t.Fatalf("a relinquished server cannot be frozen again: %v", err)
	}
	if _, err := store.IngestSamples(ctx, server.ID, []contracts.MetricSample{authoritySample(server.ID, 5)}, nil); !errors.Is(err, ErrServerNotAuthoritative) {
		t.Fatalf("relinquished server accepted ingest: %v", err)
	}
}

func TestFreezeSerializesWithConcurrentIngest(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := OpenStore(ctx, filepath.Join(dir, "authority.db"), StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := testServer()
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InitializeServerAuthority(ctx, server.ID, "owner-hub-aaaa"); err != nil {
		t.Fatal(err)
	}
	var (
		next        atomic.Uint64
		frozenFlag  atomic.Bool
		acked       atomic.Int64
		lateAckSeen atomic.Bool
		wg          sync.WaitGroup
	)
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				sequence := next.Add(1)
				startedAfterFreeze := frozenFlag.Load()
				_, err := store.IngestSamples(ctx, server.ID, []contracts.MetricSample{authoritySample(server.ID, sequence)}, nil)
				if err == nil {
					acked.Add(1)
					if startedAfterFreeze {
						lateAckSeen.Store(true)
					}
					continue
				}
				if errors.Is(err, ErrServerNotAuthoritative) {
					return
				}
				t.Errorf("unexpected ingest error: %v", err)
				return
			}
		}()
	}
	for acked.Load() < 20 {
		time.Sleep(time.Millisecond)
	}
	if _, err := store.FreezeServerAuthority(ctx, AuthorityTransition{ServerID: server.ID, CutoverID: "cutover-0001", RequestDigest: authorityDigest("a"), FrontierDigest: authorityDigest("b")}); err != nil {
		t.Fatal(err)
	}
	frozenFlag.Store(true)
	wg.Wait()
	var rows int64
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM metric_samples WHERE server_id=?`, string(server.ID)).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if lateAckSeen.Load() {
		t.Fatal("an ingest that started after the freeze returned was acknowledged")
	}
	if rows != acked.Load() {
		t.Fatalf("durable rows=%d acknowledged=%d: every acknowledged batch, and only those, must be in the final tail", rows, acked.Load())
	}
}

func TestSchemaSixDatabaseMigratesToAuthoritySchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	store, err := OpenStore(ctx, path, StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	server := testServer()
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	if _, err := store.IngestSamples(ctx, server.ID, []contracts.MetricSample{authoritySample(server.ID, 0)}, nil); err != nil {
		t.Fatal(err)
	}
	// Rewind the file to the schema-6 shape: no authority tables, version 6.
	for _, statement := range []string{`DROP TABLE server_authority_transitions`, `DROP TABLE server_authority`, `UPDATE schema_meta SET version=6`} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(ctx, path, StoreOptions{})
	if err != nil {
		t.Fatalf("schema-6 database failed to migrate: %v", err)
	}
	defer reopened.Close()
	var version int
	if err := reopened.db.QueryRowContext(ctx, `SELECT version FROM schema_meta`).Scan(&version); err != nil || version != 7 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	var rows int
	if err := reopened.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM metric_samples`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("migration changed sample rows: rows=%d err=%v", rows, err)
	}
	if _, found, err := reopened.GetServerAuthority(ctx, server.ID); err != nil || found {
		t.Fatalf("migration must not assign authority implicitly: found=%v err=%v", found, err)
	}
	if _, err := reopened.IngestSamples(ctx, server.ID, []contracts.MetricSample{authoritySample(server.ID, 1)}, nil); err != nil {
		t.Fatalf("migrated unowned server lost legacy ingest: %v", err)
	}
}
