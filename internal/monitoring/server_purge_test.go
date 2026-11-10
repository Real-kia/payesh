package monitoring

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

func TestDeleteServerAsyncHidesThenPurges(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	id := contracts.ServerID("server-purge-test-0001")
	if err := store.UpsertServer(ctx, contracts.Server{ID: id, Name: "purge", Role: "node", Architecture: "amd64", Platform: "linux", ConnectionState: "connected", FreshnessState: "fresh"}); err != nil {
		t.Fatal(err)
	}
	// More rows than one purge chunk so chunking is exercised.
	if _, err := store.db.ExecContext(ctx, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<12000)
INSERT INTO metric_rollups(server_id,metric,bucket_start,bucket_seconds,sample_count,observed_seconds,coverage) SELECT ?,'cpu',printf('%08d',i),60,1,60,'full' FROM n`, string(id)); err != nil {
		t.Fatal(err)
	}
	server, _, _ := store.GetServer(ctx, id)
	revision := server.ConfigurationRevision
	if err := store.DeleteServerAsync(ctx, id, revision+1); err == nil || err.Error() != "configuration_revision_conflict" {
		t.Fatalf("stale revision not rejected: %v", err)
	}
	if err := store.DeleteServerAsync(ctx, id, revision); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := store.GetServer(ctx, id); found && store.isDeleting(string(id)) {
		t.Fatal("deleting server is still visible")
	}
	deadline := time.Now().Add(10 * time.Second)
	for store.isDeleting(string(id)) {
		if time.Now().After(deadline) {
			t.Fatal("purge did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var left int
	store.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM servers WHERE id=?)+(SELECT COUNT(*) FROM metric_rollups WHERE server_id=?)`, string(id), string(id)).Scan(&left)
	if left != 0 {
		t.Fatalf("%d rows left after purge", left)
	}
}

func TestServerDeletionProtectsCurrentIdentity(t *testing.T) {
	for _, async := range []bool{false, true} {
		for _, tc := range []struct {
			name, role, state       string
			local, unknown, allowed bool
		}{
			{name: "former hub", role: "hub", state: "disconnected", allowed: true},
			{name: "former standalone", role: "standalone", state: "disconnected", allowed: true},
			{name: "connected former hub", role: "hub", state: "connected"},
			{name: "current hub offline", role: "hub", state: "disconnected", local: true},
			{name: "current standalone", role: "standalone", state: "connected", local: true},
			{name: "current identity with node role", role: "node", state: "disconnected", local: true},
			{name: "unknown local identity", role: "hub", state: "disconnected", unknown: true},
			{name: "ordinary node", role: "node", state: "connected", allowed: true},
		} {
			t.Run(fmt.Sprintf("%s/async=%v", tc.name, async), func(t *testing.T) {
				ctx := context.Background()
				id := contracts.ServerID("server-delete-policy-target")
				localID := contracts.ServerID("server-delete-policy-current")
				if tc.local {
					localID = id
				}
				if tc.unknown {
					localID = ""
				}
				store, err := OpenStore(ctx, ":memory:", StoreOptions{LocalServerID: localID})
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				if err := store.UpsertServer(ctx, contracts.Server{ID: id, Name: "target", Role: tc.role, Architecture: "amd64", Platform: "linux", ConnectionState: tc.state, FreshnessState: "stale", ConfigurationRevision: 7}); err != nil {
					t.Fatal(err)
				}
				if _, err := store.db.ExecContext(ctx, `INSERT INTO metric_rollups(server_id,metric,bucket_start,bucket_seconds,sample_count,observed_seconds,coverage) VALUES (?,'cpu','1',60,1,60,'full')`, string(id)); err != nil {
					t.Fatal(err)
				}
				server, found, err := store.GetServer(ctx, id)
				if err != nil || !found || server.CanDelete != tc.allowed || (server.DeletionBlockedReason == "") != tc.allowed {
					t.Fatalf("unexpected policy: %+v found=%v err=%v", server, found, err)
				}
				page, err := store.QueryServerPage(ctx, 10, "")
				if err != nil || len(page.Items) != 1 || page.Items[0].CanDelete != tc.allowed {
					t.Fatalf("list policy: %+v err=%v", page, err)
				}
				deleteServer := store.DeleteServer
				if async {
					deleteServer = store.DeleteServerAsync
				}
				if tc.allowed {
					if err := deleteServer(ctx, id, 8); err == nil || err.Error() != "configuration_revision_conflict" {
						t.Fatalf("stale revision: %v", err)
					}
				}
				err = deleteServer(ctx, id, 7)
				if !tc.allowed {
					if err == nil || err.Error() != "server_role_not_deletable" {
						t.Fatalf("protected deletion: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(5 * time.Second)
				for store.isDeleting(string(id)) {
					if time.Now().After(deadline) {
						t.Fatal("purge timed out")
					}
					time.Sleep(10 * time.Millisecond)
				}
				var records, history int
				if err := store.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM servers WHERE id=?), (SELECT COUNT(*) FROM metric_rollups WHERE server_id=?)`, string(id), string(id)).Scan(&records, &history); err != nil {
					t.Fatal(err)
				}
				want := 1
				if tc.allowed {
					want = 0
				}
				if records != want || history != want {
					t.Fatalf("records=%d history=%d want=%d", records, history, want)
				}
			})
		}
	}
}

func TestPurgePreservesReconnectedFormerMaster(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{LocalServerID: "server-current-master-0001"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	id := contracts.ServerID("server-reconnected-master")
	if err := store.UpsertServer(ctx, contracts.Server{ID: id, Name: "former", Role: "hub", Architecture: "amd64", Platform: "linux", ConnectionState: "connected", FreshnessState: "fresh"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO metric_rollups(server_id,metric,bucket_start,bucket_seconds,sample_count,observed_seconds,coverage) VALUES (?,'cpu','1',60,1,60,'full')`, string(id)); err != nil {
		t.Fatal(err)
	}
	// Simulate a master reconnecting after a delete was accepted but before purge.
	store.deleting.Store(string(id), struct{}{})
	store.purgeServer(id)
	server, found, err := store.GetServer(ctx, id)
	if err != nil || !found || server.CanDelete {
		t.Fatalf("reconnected master: %+v found=%v err=%v", server, found, err)
	}
	var history int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM metric_rollups WHERE server_id=?`, string(id)).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if history != 1 {
		t.Fatalf("reconnected master's history removed: %d", history)
	}
}
