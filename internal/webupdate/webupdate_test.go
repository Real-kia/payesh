package webupdate

import (
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
	if _, ok, _ := Take(dir); ok {
		t.Fatal("request not removed")
	}
	if err := Submit(dir, Request{Version: "1.2.4"}, now.Add(time.Hour)); err != nil {
		t.Fatalf("stale status should not block: %v", err)
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
	if err != nil || status.State != StateFailed || status.Active(time.Now()) {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}
