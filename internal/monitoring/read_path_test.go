package monitoring

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

func openDiskStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(context.Background(), filepath.Join(t.TempDir(), "payesh.db"), StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestReadsDoNotWaitForAnOpenWrite(t *testing.T) {
	store := openDiskStore(t)
	ctx := context.Background()
	if err := store.UpsertServer(ctx, testServer()); err != nil {
		t.Fatal(err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE servers SET name='Renamed' WHERE id=?`, string(testServer().ID)); err != nil {
		t.Fatal(err)
	}
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	page, err := store.QueryServerPage(readCtx, 10, "")
	if err != nil {
		t.Fatalf("read blocked behind the writer: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Name != "Local" {
		t.Fatalf("read saw uncommitted data or nothing: %+v", page.Items)
	}
	if _, found, err := store.GetServer(readCtx, testServer().ID); err != nil || !found {
		t.Fatalf("GetServer = %v, %v", found, err)
	}
}

func TestReaderPoolIsQueryOnly(t *testing.T) {
	store := openDiskStore(t)
	if store.reader == nil {
		t.Fatal("disk store has no reader pool")
	}
	if _, err := store.reader.Exec(`DELETE FROM servers`); err == nil {
		t.Fatal("reader pool accepted a write")
	}
}

func TestMemoryStoreReadsUseWriter(t *testing.T) {
	store, err := OpenStore(context.Background(), ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if store.reader != nil || store.readDB() != store.db {
		t.Fatal("in-memory store must not open a separate (empty) reader database")
	}
}

func TestRefreshServerStatesSkipsUnchangedRows(t *testing.T) {
	store := openDiskStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	server := testServer()
	server.LastHeartbeat = &now
	if err := store.UpsertServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	if err := store.RefreshServerStates(ctx, now); err != nil {
		t.Fatal(err)
	}
	var before, after int64
	if err := store.db.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := store.RefreshServerStates(ctx, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("steady refresh rewrote %d rows", after-before)
	}
	if err := store.RefreshServerStates(ctx, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, _, err := store.GetServer(ctx, server.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ConnectionState != "disconnected" || got.FreshnessState != "stale" || got.FreshnessReason != "heartbeat-timeout" {
		t.Fatalf("state after timeout = %s/%s/%s", got.ConnectionState, got.FreshnessState, got.FreshnessReason)
	}
}

func TestRefreshServerStatesForReadIsThrottled(t *testing.T) {
	store := openDiskStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	server := testServer()
	server.LastHeartbeat = &now
	if err := store.UpsertServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	if err := store.RefreshServerStatesForRead(ctx, now); err != nil {
		t.Fatal(err)
	}
	// Within the interval the heartbeat timeout is not applied yet.
	if err := store.RefreshServerStatesForRead(ctx, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE servers SET last_heartbeat=? WHERE id=?`, FormatPersistedTime(now.Add(-5*time.Minute)), string(server.ID)); err != nil {
		t.Fatal(err)
	}
	if err := store.RefreshServerStatesForRead(ctx, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := store.GetServer(ctx, server.ID); got.ConnectionState != "connected" {
		t.Fatalf("throttled refresh ran early: %s", got.ConnectionState)
	}
	if err := store.RefreshServerStatesForRead(ctx, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := store.GetServer(ctx, server.ID); got.ConnectionState != "disconnected" {
		t.Fatalf("refresh after the interval did not run: %s", got.ConnectionState)
	}
}

func TestResponseCacheHitsAndCoalesces(t *testing.T) {
	var cache responseCache
	clock := time.Unix(1000, 0)
	cache.now = func() time.Time { return clock }
	var calls atomic.Int32
	release := make(chan struct{})
	produce := func(w http.ResponseWriter) {
		calls.Add(1)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
	var wg sync.WaitGroup
	recorders := make([]*httptest.ResponseRecorder, 5)
	for i := range recorders {
		recorders[i] = httptest.NewRecorder()
		wg.Add(1)
		go func(w *httptest.ResponseRecorder) { defer wg.Done(); cache.serve(w, "k", produce) }(recorders[i])
	}
	// Let the goroutines queue on the leader before releasing it.
	for deadline := time.Now().Add(2 * time.Second); calls.Load() == 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("5 concurrent identical requests ran %d queries", calls.Load())
	}
	for _, r := range recorders {
		if r.Code != 200 || r.Body.String() != `{"ok":true}` || r.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("bad response %d %q", r.Code, r.Body.String())
		}
	}
	hit := httptest.NewRecorder()
	cache.serve(hit, "k", produce)
	if calls.Load() != 1 || hit.Header().Get("X-Payesh-Cache") != "hit" {
		t.Fatalf("expected cache hit, calls=%d state=%q", calls.Load(), hit.Header().Get("X-Payesh-Cache"))
	}
	clock = clock.Add(metricCacheTTL)
	cache.serve(httptest.NewRecorder(), "k", produce)
	if calls.Load() != 2 {
		t.Fatalf("expired entry was served, calls=%d", calls.Load())
	}
}

func TestResponseCacheDoesNotStoreErrors(t *testing.T) {
	var cache responseCache
	var calls int
	fail := func(w http.ResponseWriter) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
	}
	cache.serve(httptest.NewRecorder(), "k", fail)
	cache.serve(httptest.NewRecorder(), "k", fail)
	if calls != 2 {
		t.Fatalf("error response was cached, calls=%d", calls)
	}
}

func TestIdleJobPollDoesNotTakeWriteLock(t *testing.T) {
	store := openDiskStore(t)
	ctx := context.Background()
	if err := store.UpsertServer(ctx, testServer()); err != nil {
		t.Fatal(err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	pollCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, found, err := store.LeaseNextActionJob(pollCtx, testServer().ID, time.Now().UTC(), time.Minute); err != nil || found {
		t.Fatalf("idle poll blocked or found a job: found=%v err=%v", found, err)
	}
}

func TestStorageBudgetIgnoresReusableFreePages(t *testing.T) {
	store := openDiskStore(t)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `CREATE TABLE filler(blob BLOB); WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<2000) INSERT INTO filler SELECT randomblob(4000) FROM n; DELETE FROM filler; PRAGMA wal_checkpoint(TRUNCATE);`); err != nil {
		t.Fatal(err)
	}
	free, err := store.freePageBytes()
	if err != nil || free < 4<<20 {
		t.Fatalf("expected megabytes of free pages, got %d (%v)", free, err)
	}
	file, err := store.databaseRecoveryBytes()
	if err != nil {
		t.Fatal(err)
	}
	usage, err := store.budgetUsage(true)
	if err != nil {
		t.Fatal(err)
	}
	if usage != file-free {
		t.Fatalf("budget usage = %d, want file %d - free %d", usage, file, free)
	}
}

func TestStorageCleanupNoticesAreCombined(t *testing.T) {
	store, err := OpenStore(context.Background(), filepath.Join(t.TempDir(), "payesh.db"), StoreOptions{MaxBytes: DefaultDatabaseLimit})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := store.notifyStorageCleanup(ctx, int64(100+i), 900e6, 890e6); err != nil {
			t.Fatal(err)
		}
	}
	items, err := store.StorageNotifications(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || !strings.Contains(items[0].Message, "Removed 102") {
		t.Fatalf("notifications = %+v", items)
	}
}

func TestExpireStaleEnrollmentJobs(t *testing.T) {
	store := openDiskStore(t)
	ctx := context.Background()
	if err := store.UpsertServer(ctx, testServer()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, job := range []contracts.Job{
		{ID: "enroll-old-0001", Kind: "enrollment", State: contracts.JobQueued, IdempotencyKey: "old", TargetServerID: testServer().ID, ExpiresAt: now.Add(-time.Hour)},
		{ID: "enroll-new-0001", Kind: "enrollment", State: contracts.JobQueued, IdempotencyKey: "new", TargetServerID: testServer().ID, ExpiresAt: now.Add(time.Hour)},
	} {
		if _, _, err := store.CreateJob(ctx, job, "hash-"+job.ID, now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := store.ExpireStaleEnrollmentJobs(ctx, now); err != nil || n != 1 {
		t.Fatalf("expired %d, %v", n, err)
	}
	old, _, _ := store.GetJob(ctx, "enroll-old-0001")
	fresh, _, _ := store.GetJob(ctx, "enroll-new-0001")
	if old.State != contracts.JobFailed || old.Error == nil || old.Error.Code != "expired" || fresh.State != contracts.JobQueued {
		t.Fatalf("old=%s %+v fresh=%s", old.State, old.Error, fresh.State)
	}
}

func TestCompactIfNeededConvertsOldDatabases(t *testing.T) {
	previous := compactionMinFreeBytes
	compactionMinFreeBytes = 1 << 20
	defer func() { compactionMinFreeBytes = previous }()
	store := openDiskStore(t)
	ctx := context.Background()
	// Recreate the layout of a database made before incremental vacuum.
	if _, err := store.db.ExecContext(ctx, `PRAGMA auto_vacuum=NONE; VACUUM;`); err != nil {
		t.Fatal(err)
	}
	if result, err := store.CompactIfNeeded(ctx); err != nil || result.Ran {
		t.Fatalf("compacted a database without free space: %+v %v", result, err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TABLE filler(blob BLOB); WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<3000) INSERT INTO filler SELECT randomblob(4000) FROM n; DELETE FROM filler; PRAGMA wal_checkpoint(TRUNCATE);`); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertServer(ctx, testServer()); err != nil {
		t.Fatal(err)
	}
	result, err := store.CompactIfNeeded(ctx)
	if err != nil || !result.Ran || result.AfterBytes >= result.BeforeBytes/2 {
		t.Fatalf("compaction = %+v, %v", result, err)
	}
	var mode int
	if err := store.db.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode); err != nil || mode != 2 {
		t.Fatalf("auto_vacuum = %d, %v", mode, err)
	}
	if _, found, err := store.GetServer(ctx, testServer().ID); err != nil || !found {
		t.Fatalf("data lost by compaction: %v %v", found, err)
	}
	if again, err := store.CompactIfNeeded(ctx); err != nil || again.Ran {
		t.Fatalf("compaction ran twice: %+v %v", again, err)
	}
}

func TestDuplicateServerDetection(t *testing.T) {
	store := openDiskStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-24 * time.Hour)
	add := func(id, name, address string, connected bool, peer string) {
		server := testServer()
		server.ID, server.Name, server.Address, server.Role = contracts.ServerID(id), name, address, "node"
		server.ConnectionState, server.FreshnessState, server.LastHeartbeat = "disconnected", "stale", &old
		if connected {
			server.ConnectionState, server.FreshnessState, server.LastHeartbeat = "connected", "fresh", &now
		}
		if err := store.UpsertServer(ctx, server); err != nil {
			t.Fatal(err)
		}
		if peer != "" {
			if err := store.RecordServerPeerIP(ctx, server.ID, netip.MustParseAddr(peer)); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Stale record's address is the live node's connection source.
	add("server-stale-addr-0001", "edge old", "198.51.100.10", false, "198.51.100.10")
	add("server-live-addr-00001", "server 1", "198.51.100.20", true, "198.51.100.10")
	// Stale record's last connection source is the live node's address.
	add("server-stale-peer-0001", "relay old", "203.0.113.30", false, "203.0.113.40")
	add("server-live-peer-00001", "server 21", "203.0.113.40", true, "203.0.113.40")
	// Two different machines behind one NAT: not a duplicate.
	add("server-nat-offline-001", "office A", "10.0.0.5", false, "198.51.100.7")
	add("server-nat-online-0001", "office B", "10.0.0.6", true, "198.51.100.7")

	want := map[contracts.ServerID]contracts.ServerID{"server-stale-addr-0001": "server-live-addr-00001", "server-stale-peer-0001": "server-live-peer-00001", "server-nat-offline-001": ""}
	page, err := store.QueryServerPage(ctx, 50, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, server := range page.Items {
		expected, tracked := want[server.ID]
		if !tracked {
			if server.DuplicateOf != "" {
				t.Fatalf("connected server %s flagged as duplicate", server.ID)
			}
			continue
		}
		if server.DuplicateOf != expected {
			t.Fatalf("%s duplicate_of = %q, want %q", server.ID, server.DuplicateOf, expected)
		}
	}
	single, _, _ := store.GetServer(ctx, "server-stale-addr-0001")
	if single.DuplicateOf != "server-live-addr-00001" || single.DuplicateOfName != "server 1" {
		t.Fatalf("GetServer duplicate = %q %q", single.DuplicateOf, single.DuplicateOfName)
	}
}

func TestUnusedRelayAccessIsRemoved(t *testing.T) {
	store := openDiskStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	relay := testServer()
	relay.ID, relay.Address, relay.Role = "server-relay-clean-01", "203.0.113.5", "node"
	if err := store.UpsertServer(ctx, relay); err != nil {
		t.Fatal(err)
	}
	for _, id := range []contracts.ServerID{"server-via-relay-0001", "server-direct-node-01"} {
		node := testServer()
		node.ID, node.Role = id, "node"
		if err := store.UpsertServer(ctx, node); err != nil {
			t.Fatal(err)
		}
	}
	created := now.Add(-40 * 24 * time.Hour)
	if _, err := store.EnsureRelayNode(ctx, relay.ID, DefaultRelayPort, created); err != nil {
		t.Fatal(err)
	}
	for _, id := range []contracts.ServerID{"server-via-relay-0001", "server-direct-node-01"} {
		if err := store.PutRelayGrant(ctx, RelayGrantRecord{RelayServerID: relay.ID, Username: "node-" + string(id), PasswordSHA256: strings.Repeat("a", 64), Destinations: []string{"hub:9797"}, TargetServerID: id}, created); err != nil {
			t.Fatal(err)
		}
	}
	// One node connects from the relay's address; the other connects directly.
	if err := store.TouchRelayUse(ctx, "server-via-relay-0001", "203.0.113.5", now); err != nil {
		t.Fatal(err)
	}
	if err := store.TouchRelayUse(ctx, "server-direct-node-01", "198.51.100.77", now); err != nil {
		t.Fatal(err)
	}
	if removed, err := store.PruneUnusedRelayGrants(ctx, now, 0); err != nil || removed != 0 {
		t.Fatalf("days=0 must keep everything: %d %v", removed, err)
	}
	removed, err := store.PruneUnusedRelayGrants(ctx, now, 30)
	if err != nil || removed != 1 {
		t.Fatalf("removed %d, %v", removed, err)
	}
	policy, _ := store.RelayPolicy(ctx, relay.ID, now)
	if len(policy.Grants) != 1 || policy.Grants[0].Username != "node-server-via-relay-0001" {
		t.Fatalf("kept grants: %+v", policy.Grants)
	}
}

func TestRelayCleanupSettingRoundTrip(t *testing.T) {
	store, err := OpenStore(context.Background(), filepath.Join(t.TempDir(), "payesh.db"), StoreOptions{MaxBytes: DefaultDatabaseLimit})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	settings, _, err := store.StorageSettings(ctx)
	if err != nil || settings.RelayCleanupDays != 30 {
		t.Fatalf("default relay cleanup = %d, %v", settings.RelayCleanupDays, err)
	}
	settings.RelayCleanupDays = 7
	if err := store.SaveStorageSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	if again, _, _ := store.StorageSettings(ctx); again.RelayCleanupDays != 7 {
		t.Fatalf("saved relay cleanup = %d", again.RelayCleanupDays)
	}
	again, _, _ := store.StorageSettings(ctx)
	again.RelayCleanupDays = -1
	if err := store.SaveStorageSettings(ctx, again); err == nil {
		t.Fatal("negative days accepted")
	}
}
