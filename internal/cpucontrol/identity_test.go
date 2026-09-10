package cpucontrol

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// writeFakeStat writes a /proc/<pid>/stat fixture with a realistic comm
// field (including spaces and parentheses, which real process names can
// contain) and the given starttime at field 22.
func writeFakeStat(t *testing.T, procRoot string, pid int, comm string, startTicks uint64) {
	t.Helper()
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// pid (comm) state ppid pgrp session tty_nr tpgid flags minflt cminflt
	// majflt cmajflt utime stime cutime cstime priority nice num_threads
	// itrealvalue starttime ...
	line := strconv.Itoa(pid) + " (" + comm + ") S 1 1 1 0 -1 4194304 0 0 0 0 0 0 0 0 20 0 1 0 " + strconv.FormatUint(startTicks, 10) + " 0 0\n"
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadProcessIdentityParsesStartTicks(t *testing.T) {
	root := t.TempDir()
	writeFakeStat(t, root, 4242, "nginx", 987654)
	identity, err := ReadProcessIdentity(root, 4242)
	if err != nil {
		t.Fatal(err)
	}
	if identity.PID != 4242 || identity.StartTicks != 987654 {
		t.Fatalf("unexpected identity: %+v", identity)
	}
}

func TestReadProcessIdentityHandlesCommWithSpacesAndParens(t *testing.T) {
	root := t.TempDir()
	writeFakeStat(t, root, 99, "some (weird) name", 111)
	identity, err := ReadProcessIdentity(root, 99)
	if err != nil {
		t.Fatal(err)
	}
	if identity.StartTicks != 111 {
		t.Fatalf("expected starttime 111 despite parens in comm, got %d", identity.StartTicks)
	}
}

func TestVerifyIdentityDetectsPIDReuse(t *testing.T) {
	root := t.TempDir()
	writeFakeStat(t, root, 4242, "nginx", 1000)
	expected := ProcessIdentity{PID: 4242, StartTicks: 1000}
	ok, err := VerifyIdentity(root, expected)
	if err != nil || !ok {
		t.Fatalf("expected identity to verify, ok=%v err=%v", ok, err)
	}
	// The original process exits; the kernel reuses PID 4242 for an
	// unrelated later process with a different start time.
	writeFakeStat(t, root, 4242, "some-other-process", 5000)
	ok, err = VerifyIdentity(root, expected)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected a reused PID with a different start time to fail verification")
	}
}

func TestVerifyIdentityHandlesExitedProcess(t *testing.T) {
	root := t.TempDir()
	ok, err := VerifyIdentity(root, ProcessIdentity{PID: 12345, StartTicks: 1})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected a missing /proc entry to fail verification, not error")
	}
}
