package webupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSubmitTakeAndBusy(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	if err := Submit(dir, Request{Version: "not a version"}, now); err == nil {
		t.Fatal("invalid version accepted")
	}
	if err := Submit(dir, Request{Version: "1.2.3"}, now); err != nil {
		t.Fatal(err)
	}
	if err := Submit(dir, Request{Version: "1.2.4"}, now.Add(time.Minute)); !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	req, ok, err := Take(dir)
	if err != nil || !ok || req.Version != "1.2.3" {
		t.Fatalf("take = %+v %v %v", req, ok, err)
	}
	retained, found, err := Take(dir)
	if err != nil || !found || retained != req || req.JobID == "" || !req.Deadline.Equal(now.UTC().Add(LocalAuthorizationTTL)) {
		t.Fatalf("durable intent not retained: %+v %v %v", retained, found, err)
	}
	if err := Submit(dir, Request{Version: "1.2.4"}, now.Add(time.Hour)); !errors.Is(err, ErrBusy) {
		t.Fatalf("stale status must not overwrite recovery intent: %v", err)
	}
	if _, err := RunFleet(context.Background(), dir, req, "1.0.0", func(context.Context, string) error { return nil }, func(context.Context, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := Submit(dir, Request{Version: "1.2.4"}, now.Add(time.Hour)); err != nil {
		t.Fatalf("terminal intent should permit a new request: %v", err)
	}
}

func TestSymlinkRequestRejected(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "x")
	_ = os.WriteFile(target, []byte(`{"version":"1.2.3"}`), 0o600)
	if err := os.Symlink(target, filepath.Join(dir, RequestFile)); err != nil {
		t.Skip(err)
	}
	if _, ok, err := Take(dir); ok || err == nil {
		t.Fatalf("symlink request accepted: %v %v", ok, err)
	}
}

func TestStatusWriteReplacesSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	_ = os.WriteFile(victim, []byte("keep"), 0o600)
	if err := os.Symlink(victim, filepath.Join(dir, StatusFile)); err != nil {
		t.Skip(err)
	}
	if err := WriteStatus(dir, Status{State: StateRunning, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatal("symlink target overwritten")
	}
}

func TestConcurrentSubmitQueuesOnlyOneRequest(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	results := make(chan error, 12)
	for i := 0; i < 12; i++ {
		go func() { results <- Submit(dir, Request{Version: "1.2.3"}, now) }()
	}
	successes := 0
	for i := 0; i < 12; i++ {
		err := <-results
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrBusy) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("accepted %d requests", successes)
	}
	request, ok, err := Take(dir)
	if err != nil || !ok || request.Version != "1.2.3" {
		t.Fatalf("request=%+v found=%v err=%v", request, ok, err)
	}
}

func TestFailedRequestWriteDoesNotLeaveActiveStatus(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, RequestFile), 0700); err != nil {
		t.Fatal(err)
	}
	if err := Submit(dir, Request{Version: "1.2.3"}, time.Now()); err == nil {
		t.Fatal("expected request write failure")
	}
	status, err := ReadStatus(dir)
	if !errors.Is(err, os.ErrNotExist) && (err != nil || status.Active(time.Now())) {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}

func TestHistoricalBrowserIntentPromotionRetainsIdentityAndAuthorization(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	original := Request{Version: "1.2.3", RequestedAt: now.Add(-time.Minute), RequestedBy: "owner"}
	if err := writeJSON(dir, RequestFile, original, 0600); err != nil {
		t.Fatal(err)
	}
	promoted, found, err := takeAt(dir, now)
	if err != nil || !found || promoted.JobID == "" || promoted.RequestedAt != original.RequestedAt || promoted.RequestedBy != original.RequestedBy || !promoted.Deadline.Equal(original.RequestedAt.Add(LocalAuthorizationTTL)) {
		t.Fatalf("promotion=%+v found=%v err=%v", promoted, found, err)
	}
	for i := 0; i < 3; i++ {
		got, found, err := takeAt(dir, now.Add(time.Duration(i)*time.Minute))
		if err != nil || !found || got != promoted {
			t.Fatalf("promotion changed on retry: %+v %v %v", got, found, err)
		}
	}
	// An older worker may leave the original bytes after interruption; the
	// same authorization must still map to the same protected root journal.
	if err := writeJSON(dir, RequestFile, original, 0600); err != nil {
		t.Fatal(err)
	}
	got, found, err := takeAt(dir, now)
	if err != nil || !found || got != promoted {
		t.Fatalf("replayed legacy intent=%+v %v %v", got, found, err)
	}
}

func TestHistoricalBrowserExpiryNeverGrantsNewAuthorization(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	original := Request{Version: "1.2.3", RequestedAt: now.Add(-time.Hour), Deadline: now.Add(time.Hour)}
	if err := writeJSON(dir, RequestFile, original, 0600); err != nil {
		t.Fatal(err)
	}
	req, found, err := takeAt(dir, now)
	if err != nil || !found || req.Deadline.After(now) {
		t.Fatalf("old intent gained authorization: %+v %v %v", req, found, err)
	}
	called := false
	done, err := RunFleet(context.Background(), dir, req, "1.0.0", func(context.Context, string) error { called = true; return nil }, func(context.Context, string) error { called = true; return nil })
	if done || err == nil || called {
		t.Fatalf("expired activation done=%v err=%v called=%v", done, err, called)
	}
	result, err := FleetResult(dir, req.JobID)
	if err != nil || result.State != StateFailed {
		t.Fatalf("expired result=%+v err=%v", result, err)
	}
	if _, found, err := Take(dir); err != nil || found {
		t.Fatalf("terminal expired intent=%v %v", found, err)
	}
}

func TestHistoricalBrowserMissingOrFutureAuthorizationPreservesRequest(t *testing.T) {
	for _, requested := range []time.Time{{}, time.Now().UTC().Add(time.Hour)} {
		dir := t.TempDir()
		original := Request{Version: "1.2.3", RequestedAt: requested}
		if err := writeJSON(dir, RequestFile, original, 0600); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(filepath.Join(dir, RequestFile))
		if err != nil {
			t.Fatal(err)
		}
		if _, found, err := Take(dir); err == nil || found {
			t.Fatalf("invalid authorization accepted: %v %v", found, err)
		}
		after, err := os.ReadFile(filepath.Join(dir, RequestFile))
		if err != nil || string(after) != string(before) {
			t.Fatalf("invalid authorization lost recovery evidence: %v", err)
		}
	}
}

func TestConcurrentHistoricalPromotionUsesOneDurableIdentity(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	if err := writeJSON(dir, RequestFile, Request{Version: "1.2.3", RequestedAt: now.Add(-time.Minute)}, 0600); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		request Request
		found   bool
		err     error
	}
	results := make(chan outcome, 12)
	for i := 0; i < 12; i++ {
		go func() { request, found, err := takeAt(dir, now); results <- outcome{request, found, err} }()
	}
	var first Request
	for i := 0; i < 12; i++ {
		result := <-results
		if result.err != nil || !result.found || result.request.JobID == "" {
			t.Fatalf("promotion=%+v", result)
		}
		if i == 0 {
			first = result.request
		} else if first != result.request {
			t.Fatalf("different recovery identities: %+v %+v", first, result.request)
		}
	}
}
