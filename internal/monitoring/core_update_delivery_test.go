package monitoring

import (
	"context"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

func coreDeliveryFixture(t *testing.T) (*Store, contracts.ServerID, time.Time) {
	t.Helper()
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	server := testServer()
	if err := store.EnsureServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	parent := contracts.Job{ID: "parent-core-delivery", Kind: "update", State: contracts.JobRunning, IdempotencyKey: "parent-core-delivery", ExpiresAt: now.Add(time.Hour)}
	child := contracts.Job{ID: "child-core-delivery", Kind: "update-node", State: contracts.JobRunning, IdempotencyKey: "child-core-delivery", TargetServerID: server.ID, ExpiresAt: now.Add(time.Hour), Action: &contracts.ActionRequest{Protocol: contracts.HelperProtocol, RequestID: "child-core-delivery", Action: "update", TargetServerID: server.ID, IdempotencyKey: "child-core-delivery", Target: "payesh-agent", Arguments: []byte(`{"parent_job_id":"parent-core-delivery","release":"1.2.0"}`), Deadline: now.Add(time.Hour)}}
	action := contracts.Job{ID: "action-core-delivery", Kind: "core-update-action", State: contracts.JobQueued, IdempotencyKey: "action-core-delivery", TargetServerID: server.ID, ExpiresAt: now.Add(time.Hour), Action: &contracts.ActionRequest{Protocol: contracts.HelperProtocol, RequestID: "action-core-delivery", Action: "core.update", TargetServerID: server.ID, IdempotencyKey: "action-core-delivery", Target: "payesh-core", Arguments: []byte(`{"job_id":"child-core-delivery","release":"1.2.0"}`), Deadline: now.Add(time.Hour)}}
	for _, job := range []contracts.Job{parent, child, action} {
		if _, _, err := store.CreateJob(ctx, job, job.ID, now); err != nil {
			t.Fatal(err)
		}
	}
	return store, server.ID, now
}

func TestCoreDeliveryRequiresCurrentCorrelatedAuthorization(t *testing.T) {
	cases := map[string]string{
		"parent expired":      `UPDATE jobs SET expires_at=? WHERE id='parent-core-delivery'`,
		"child expired":       `UPDATE jobs SET expires_at=? WHERE id='child-core-delivery'`,
		"parent cancelled":    `UPDATE jobs SET cancel_requested=1 WHERE id='parent-core-delivery'`,
		"child cancelled":     `UPDATE jobs SET cancel_requested=1 WHERE id='child-core-delivery'`,
		"parent terminal":     `UPDATE jobs SET state='succeeded' WHERE id='parent-core-delivery'`,
		"child terminal":      `UPDATE jobs SET state='failed' WHERE id='child-core-delivery'`,
		"child unclaimed":     `UPDATE jobs SET state='queued' WHERE id='child-core-delivery'`,
		"parent wrong kind":   `UPDATE jobs SET kind='module-action' WHERE id='parent-core-delivery'`,
		"child wrong kind":    `UPDATE jobs SET kind='module-action' WHERE id='child-core-delivery'`,
		"core disguised kind": `UPDATE jobs SET kind='module-action' WHERE id='action-core-delivery'`,
		"missing parent":      `DELETE FROM jobs WHERE id='parent-core-delivery'`,
		"missing child":       `DELETE FROM jobs WHERE id='child-core-delivery'`,
		"mismatched target":   `UPDATE jobs SET target_server_id='server-other-target' WHERE id='child-core-delivery'`,
		"mismatched release":  `UPDATE jobs SET action_json=json_set(action_json,'$.arguments.release','1.3.0') WHERE id='child-core-delivery'`,
		"revoked node":        `UPDATE servers SET connection_state='revoked'`,
	}
	for name, query := range cases {
		t.Run(name, func(t *testing.T) {
			store, node, now := coreDeliveryFixture(t)
			var err error
			if name == "parent expired" || name == "child expired" {
				_, err = store.db.Exec(query, FormatPersistedTime(now))
			} else {
				_, err = store.db.Exec(query)
			}
			if err != nil {
				t.Fatal(err)
			}
			if lease, ok, err := store.LeaseNextActionJob(context.Background(), node, now, time.Minute); err != nil || ok {
				t.Fatalf("unauthorized delivery: %+v %v %v", lease, ok, err)
			}
		})
	}
}

func TestCoreDeliveryRechecksAuthorizationBeforeLeaseRedelivery(t *testing.T) {
	store, node, now := coreDeliveryFixture(t)
	if _, ok, err := store.LeaseNextActionJob(context.Background(), node, now, time.Minute); err != nil || !ok {
		t.Fatalf("authorized initial delivery: %v %v", ok, err)
	}
	// Change ancestry without cascading into the existing action: the transport
	// boundary itself must guard reconnect/reclaim, independently of scheduler.
	if _, err := store.db.Exec(`UPDATE jobs SET cancel_requested=1 WHERE id='parent-core-delivery'`); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.LeaseNextActionJob(context.Background(), node, now.Add(2*time.Minute), time.Minute); err != nil || ok {
		t.Fatalf("cancelled lease redelivered: %v %v", ok, err)
	}
}
