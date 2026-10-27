package monitoring

import (
	"context"
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
