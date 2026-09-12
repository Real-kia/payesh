package updater

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func cutoverDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := t.TempDir() + "/journal.db"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func hooksRecording(calls *[]string, fail string) RoleCutoverHooks {
	h := func(name string) func(context.Context, RoleCutoverRequest) error {
		return func(context.Context, RoleCutoverRequest) error {
			*calls = append(*calls, name)
			if name == fail {
				return errors.New("injected")
			}
			return nil
		}
	}
	return RoleCutoverHooks{Preflight: h("preflight"), Backup: h("backup"), Transfer: h("transfer"), Verify: h("verify"), Freeze: h("freeze"), TransferTail: h("tail"), SwitchAuthority: h("switch"), RevokeOldAuthority: h("revoke"), Confirm: h("confirm"), Cleanup: h("cleanup"), RollbackBeforeSwitch: h("rollback")}
}

func testCutoverRequest() RoleCutoverRequest {
	return RoleCutoverRequest{ID: "cutover-0123456789", SourceRole: "standalone", DestinationRole: "node", SourceID: "server-0123456789", DestinationID: "hub-01234567890123", CleanupSource: true}
}

func TestRoleCutoverCompletesInOrderAndIsIdempotent(t *testing.T) {
	db, _ := cutoverDB(t)
	var calls []string
	now := func() time.Time { return time.Date(2026, 9, 12, 1, 2, 3, 0, time.UTC) }
	record, err := RunRoleCutover(context.Background(), db, testCutoverRequest(), hooksRecording(&calls, ""), now)
	if err != nil || record.Phase != CutoverComplete {
		t.Fatalf("record=%+v err=%v", record, err)
	}
	want := []string{"preflight", "backup", "transfer", "verify", "freeze", "tail", "switch", "revoke", "confirm", "cleanup"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%v", calls)
	}
	_, err = RunRoleCutover(context.Background(), db, testCutoverRequest(), hooksRecording(&calls, ""), now)
	if err != nil || !reflect.DeepEqual(calls, want) {
		t.Fatalf("replay calls=%v err=%v", calls, err)
	}
}

func TestRoleCutoverResumesBeforeSwitchAndFailsClosedAfterIt(t *testing.T) {
	db, path := cutoverDB(t)
	req := testCutoverRequest()
	now := func() time.Time { return time.Now().UTC() }
	var first []string
	record, err := RunRoleCutover(context.Background(), db, req, hooksRecording(&first, "tail"), now)
	if err == nil || record.Phase != CutoverTail || first[len(first)-1] != "rollback" {
		t.Fatalf("record=%+v calls=%v err=%v", record, first, err)
	}
	_ = db.Close()
	reopened, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var resumed []string
	record, err = RunRoleCutover(context.Background(), reopened, req, hooksRecording(&resumed, "revoke"), now)
	if err == nil || record.Phase != CutoverRecovery {
		t.Fatalf("record=%+v calls=%v err=%v", record, resumed, err)
	}
	if reflect.DeepEqual(resumed, []string{"rollback"}) || len(resumed) != 3 || resumed[0] != "tail" || resumed[1] != "switch" || resumed[2] != "revoke" {
		t.Fatalf("resume calls=%v", resumed)
	}
	var retry []string
	_, err = RunRoleCutover(context.Background(), reopened, req, hooksRecording(&retry, ""), now)
	if err == nil || len(retry) != 0 {
		t.Fatalf("recovery state executed hooks: %v err=%v", retry, err)
	}
}

func TestRoleCutoverBlocksManagedHubAndConflictingReplay(t *testing.T) {
	db, _ := cutoverDB(t)
	req := testCutoverRequest()
	req.SourceRole = "hub"
	req.ManagedNodes = 2
	if _, err := RunRoleCutover(context.Background(), db, req, RoleCutoverHooks{}, time.Now); err == nil {
		t.Fatal("managed hub transition accepted")
	}
	req = testCutoverRequest()
	req.CleanupSource = false
	var calls []string
	if _, err := RunRoleCutover(context.Background(), db, req, hooksRecording(&calls, ""), time.Now); err != nil {
		t.Fatal(err)
	}
	req.DestinationID = "different-hub-1234"
	if _, err := RunRoleCutover(context.Background(), db, req, hooksRecording(&calls, ""), time.Now); err == nil {
		t.Fatal("idempotency conflict accepted")
	}
}
