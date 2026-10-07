package updater

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
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
	if err == nil || record.Phase != CutoverPreflight || first[len(first)-1] != "rollback" {
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
	if !reflect.DeepEqual(resumed, []string{"preflight", "backup", "transfer", "verify", "freeze", "tail", "switch", "revoke"}) {
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

func TestRoleCutoverExpiredOwnerCannotOverwriteSuccessor(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			db, _ := cutoverDB(t)
			var clock atomic.Int64
			clock.Store(time.Date(2026, 9, 12, 1, 2, 3, 0, time.UTC).UnixNano())
			now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
			entered, release := make(chan struct{}), make(chan struct{})
			firstDone := make(chan error, 1)
			var firstCalls []string
			firstHooks := hooksRecording(&firstCalls, "")
			firstHooks.Preflight = func(context.Context, RoleCutoverRequest) error {
				close(entered)
				<-release
				if failure {
					return errors.New("old owner failure")
				}
				return nil
			}
			go func() {
				_, err := RunRoleCutover(context.Background(), db, testCutoverRequest(), firstHooks, now)
				firstDone <- err
			}()
			<-entered
			clock.Add(int64(6 * time.Minute))
			var successorCalls []string
			record, err := RunRoleCutover(context.Background(), db, testCutoverRequest(), hooksRecording(&successorCalls, "revoke"), now)
			close(release)
			staleErr := <-firstDone
			if err == nil || record.Phase != CutoverRecovery {
				t.Fatalf("successor=%+v err=%v", record, err)
			}
			if staleErr == nil || !strings.Contains(staleErr.Error(), "lease") {
				t.Fatalf("expired owner err=%v", staleErr)
			}
			stored, _, err := loadCutover(context.Background(), db, testCutoverRequest().ID)
			if err != nil || stored.Phase != CutoverRecovery || stored.LastError != record.LastError {
				t.Fatalf("stale owner changed successor: %+v err=%v", stored, err)
			}
			if len(firstCalls) != 0 {
				t.Fatalf("stale owner executed later hooks/rollback: %v", firstCalls)
			}
		})
	}
}

func TestRoleCutoverCanceledTailRollsBackAndPersistsRestart(t *testing.T) {
	db, path := cutoverDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls []string
	hooks := hooksRecording(&calls, "")
	hooks.TransferTail = func(context.Context, RoleCutoverRequest) error { cancel(); return ctx.Err() }
	hooks.RollbackBeforeSwitch = func(recovery context.Context, _ RoleCutoverRequest) error {
		if recovery.Err() != nil {
			return recovery.Err()
		}
		deadline, ok := recovery.Deadline()
		if !ok || time.Until(deadline) > time.Minute {
			return errors.New("rollback lacks bounded independent context")
		}
		calls = append(calls, "rollback")
		return nil
	}
	record, err := RunRoleCutover(ctx, db, testCutoverRequest(), hooks, time.Now)
	if err == nil || record.Phase != CutoverPreflight || calls[len(calls)-1] != "rollback" {
		t.Fatalf("record=%+v calls=%v err=%v", record, calls, err)
	}
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	stored, _, err := loadCutover(context.Background(), other, testCutoverRequest().ID)
	if err != nil || stored.Phase != CutoverPreflight || !strings.Contains(stored.LastError, "canceled") {
		t.Fatalf("restart state=%+v err=%v", stored, err)
	}
	var retried []string
	if _, err := RunRoleCutover(context.Background(), other, testCutoverRequest(), hooksRecording(&retried, ""), time.Now); err != nil || retried[0] != "preflight" {
		t.Fatalf("retry=%v err=%v", retried, err)
	}
}

func TestRoleCutoverRollbackFailureRequiresRecovery(t *testing.T) {
	db, _ := cutoverDB(t)
	var calls []string
	hooks := hooksRecording(&calls, "tail")
	hooks.RollbackBeforeSwitch = func(context.Context, RoleCutoverRequest) error { return errors.New("source still frozen") }
	record, err := RunRoleCutover(context.Background(), db, testCutoverRequest(), hooks, time.Now)
	if err == nil || record.Phase != CutoverRecovery || !strings.Contains(record.LastError, "source still frozen") {
		t.Fatalf("record=%+v err=%v", record, err)
	}
	var retried []string
	if _, err := RunRoleCutover(context.Background(), db, testCutoverRequest(), hooksRecording(&retried, ""), time.Now); err == nil || len(retried) != 0 {
		t.Fatalf("unsafe retry=%v err=%v", retried, err)
	}
}

func TestRoleCutoverConcurrentInitialRequestsAreImmutable(t *testing.T) {
	db, path := cutoverDB(t)
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		t.Fatal(err)
	}
	if err := ensureCutoverJournal(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.SetMaxOpenConns(1)
	if _, err := other.Exec("PRAGMA busy_timeout=5000"); err != nil {
		t.Fatal(err)
	}
	ready, release := make(chan struct{}, 2), make(chan struct{})
	results := make(chan error, 2)
	for i, journal := range []*sql.DB{db, other} {
		req := testCutoverRequest()
		req.DestinationID += fmt.Sprint(i)
		go func() {
			var first atomic.Bool
			now := func() time.Time {
				if first.CompareAndSwap(false, true) {
					ready <- struct{}{}
					<-release
				}
				return time.Now().UTC()
			}
			var calls []string
			_, err := RunRoleCutover(context.Background(), journal, req, hooksRecording(&calls, ""), now)
			results <- err
		}()
	}
	<-ready
	<-ready
	close(release)
	var successes, conflicts int
	for range 2 {
		err := <-results
		if err == nil {
			successes++
		} else if strings.Contains(err.Error(), "idempotency conflict") {
			conflicts++
		} else {
			t.Errorf("unexpected error: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
}

func TestRoleCutoverExpiredOwnerCannotCompleteWithoutSuccessor(t *testing.T) {
	db, _ := cutoverDB(t)
	var clock atomic.Int64
	clock.Store(time.Date(2026, 9, 12, 1, 2, 3, 0, time.UTC).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	var calls []string
	hooks := hooksRecording(&calls, "")
	hooks.Preflight = func(context.Context, RoleCutoverRequest) error { clock.Add(int64(cutoverLeaseDuration)); return nil }
	record, err := RunRoleCutover(context.Background(), db, testCutoverRequest(), hooks, now)
	if !errors.Is(err, errCutoverLeaseLost) || record.Phase != CutoverPreflight || len(calls) != 0 {
		t.Fatalf("expired owner record=%+v calls=%v err=%v", record, calls, err)
	}
	var resumed []string
	if _, err := RunRoleCutover(context.Background(), db, testCutoverRequest(), hooksRecording(&resumed, ""), now); err != nil {
		t.Fatal(err)
	}
}

func TestRoleCutoverCanceledSwitchPersistsRecoveryWithoutRollback(t *testing.T) {
	db, path := cutoverDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls []string
	hooks := hooksRecording(&calls, "")
	hooks.SwitchAuthority = func(context.Context, RoleCutoverRequest) error { cancel(); return ctx.Err() }
	record, err := RunRoleCutover(ctx, db, testCutoverRequest(), hooks, time.Now)
	if !errors.Is(err, context.Canceled) || record.Phase != CutoverRecovery {
		t.Fatalf("record=%+v err=%v", record, err)
	}
	for _, call := range calls {
		if call == "rollback" || call == "revoke" {
			t.Fatalf("post-switch cancellation executed %s", call)
		}
	}
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	stored, _, err := loadCutover(context.Background(), other, testCutoverRequest().ID)
	if err != nil || stored.Phase != CutoverRecovery {
		t.Fatalf("restart state=%+v err=%v", stored, err)
	}
}

func TestRoleCutoverLegacyJournalHonorsOutstandingLease(t *testing.T) {
	db, _ := cutoverDB(t)
	at := time.Date(2026, 9, 12, 1, 2, 3, 0, time.UTC)
	if _, err := db.Exec(`CREATE TABLE role_cutovers (id TEXT PRIMARY KEY, request_json BLOB NOT NULL, phase TEXT NOT NULL, last_error TEXT NOT NULL, updated_at TEXT NOT NULL, busy_until TEXT)`); err != nil {
		t.Fatal(err)
	}
	req := testCutoverRequest()
	encoded, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO role_cutovers VALUES(?,?,?,?,?,?)`, req.ID, encoded, string(CutoverPreflight), "", at.Format(time.RFC3339Nano), at.Add(time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	var calls []string
	if _, err := RunRoleCutover(context.Background(), db, req, hooksRecording(&calls, ""), func() time.Time { return at }); err == nil || len(calls) != 0 {
		t.Fatalf("legacy lease ignored: calls=%v err=%v", calls, err)
	}
	if _, err := RunRoleCutover(context.Background(), db, req, hooksRecording(&calls, ""), func() time.Time { return at.Add(time.Minute) }); err != nil {
		t.Fatal(err)
	}
}

func TestRoleCutoverPersistenceFailureIsReturned(t *testing.T) {
	db, _ := cutoverDB(t)
	var calls []string
	hooks := hooksRecording(&calls, "")
	hooks.Preflight = func(context.Context, RoleCutoverRequest) error { return db.Close() }
	if _, err := RunRoleCutover(context.Background(), db, testCutoverRequest(), hooks, time.Now); err == nil {
		t.Fatal("phase persistence failure was ignored")
	}
	if len(calls) != 0 {
		t.Fatalf("hooks continued after persistence failure: %v", calls)
	}
}

func TestRoleCutoverPersistsRecoveryBeforeRollbackSideEffects(t *testing.T) {
	db, path := cutoverDB(t)
	var calls []string
	hooks := hooksRecording(&calls, "tail")
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	hooks.RollbackBeforeSwitch = func(context.Context, RoleCutoverRequest) error {
		close(entered)
		<-release
		return nil
	}
	go func() {
		_, err := RunRoleCutover(context.Background(), db, testCutoverRequest(), hooks, time.Now)
		done <- err
	}()
	<-entered
	other, err := sql.Open("sqlite", path)
	if err != nil {
		close(release)
		<-done
		t.Fatal(err)
	}
	defer other.Close()
	stored, _, loadErr := loadCutover(context.Background(), other, testCutoverRequest().ID)
	var retryCalls []string
	_, retryErr := RunRoleCutover(context.Background(), other, testCutoverRequest(), hooksRecording(&retryCalls, ""), time.Now)
	close(release)
	firstErr := <-done
	if loadErr != nil || stored.Phase != CutoverRecovery || !strings.Contains(stored.LastError, "rollback pending") {
		t.Fatalf("rollback intent=%+v err=%v", stored, loadErr)
	}
	if retryErr == nil || len(retryCalls) != 0 {
		t.Fatalf("incomplete rollback resumed: calls=%v err=%v", retryCalls, retryErr)
	}
	if firstErr == nil {
		t.Fatal("original tail failure lost")
	}
	stored, _, err = loadCutover(context.Background(), other, testCutoverRequest().ID)
	if err != nil || stored.Phase != CutoverPreflight {
		t.Fatalf("successful rollback state=%+v err=%v", stored, err)
	}
}
