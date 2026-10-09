package monitoring

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadConfiguredLogBoundsMultilineAndRedacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	content := "2026-09-09T10:00:00Z [INFO] started\n  continuation password=still-secret\n2026-09-09T10:00:01Z [ERROR] password=super-secret failed\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := ReadConfiguredLog(context.Background(), LogSource{ServerID: "server-local-0001", ID: "service", Label: "Service", Path: path}, LogReadOptions{MaxEntries: 10, Now: func() time.Time { return time.Date(2026, 9, 9, 10, 1, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 2 {
		t.Fatalf("multiline log was not grouped: %#v", result)
	}
	if result.Entries[0].Text != "started\ncontinuation password=[REDACTED]" || !result.Entries[0].Redacted {
		t.Fatalf("unexpected multiline text: %q", result.Entries[0].Text)
	}
	if !result.Entries[1].Redacted || result.Entries[1].Text == "" || result.Entries[1].Text == "password=super-secret failed" {
		t.Fatalf("credential was not redacted: %#v", result.Entries[1])
	}
	if result.Entries[0].Cursor == "" || len(result.Entries[0].Cursor) > 256 {
		t.Fatalf("invalid bounded cursor: %#v", result.Entries[0])
	}
}

func TestTimestampLessLogUsesQueryUpperBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	if err := os.WriteFile(path, []byte("service started\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	upper := time.Now().UTC().Add(-time.Second)
	result, err := ReadConfiguredLog(context.Background(), LogSource{ServerID: "server-local-0001", ID: "service", Label: "Service", Path: path}, LogReadOptions{MaxEntries: 10, From: upper.Add(-time.Hour), To: upper})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 1 || !result.Entries[0].Timestamp.Equal(upper) {
		t.Fatalf("timestamp-less entry fell outside snapshot: %#v", result.Entries)
	}
}

func TestReadConfiguredLogRejectsSymlinkAndCapsLines(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.log")
	link := filepath.Join(dir, "link.log")
	if err := os.WriteFile(target, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := ReadConfiguredLog(context.Background(), LogSource{ServerID: "server-local-0001", ID: "link", Label: "Link", Path: link}, LogReadOptions{}); err == nil {
		t.Fatal("symlink log source accepted")
	}
	longPath := filepath.Join(dir, "long.log")
	if err := os.WriteFile(longPath, []byte("[INFO] "+string(make([]byte, 1024))+
		"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := ReadConfiguredLog(context.Background(), LogSource{ServerID: "server-local-0001", ID: "long", Label: "Long", Path: longPath}, LogReadOptions{MaxLineSize: 64, MaxEntries: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 1 || !result.Entries[0].Truncated {
		t.Fatalf("long line was not explicitly truncated: %#v", result)
	}
	if _, err := ReadConfiguredLog(context.Background(), LogSource{ServerID: "server-local-0001", ID: "relative", Label: "Relative", Path: "relative.log"}, LogReadOptions{}); err == nil {
		t.Fatal("relative log source accepted")
	}
	boundedPath := filepath.Join(dir, "bounded.log")
	if err := os.WriteFile(boundedPath, bytes.Repeat([]byte("x"), (1<<20)+1024), 0o600); err != nil {
		t.Fatal(err)
	}
	bounded, err := ReadConfiguredLog(context.Background(), LogSource{ServerID: "server-local-0001", ID: "bounded", Label: "Bounded", Path: boundedPath}, LogReadOptions{MaxBytes: 1024, MaxLineSize: 64})
	if err != nil {
		t.Fatal(err)
	}
	if !bounded.Truncated || len(bounded.Entries) > 1 || (len(bounded.Entries) == 1 && len(bounded.Entries[0].Text) > 64) {
		t.Fatalf("log scanner exceeded byte/line bounds: %#v", bounded)
	}
}

func TestTailConfiguredLogHonorsCursorAndWaitBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tail.log")
	if err := os.WriteFile(path, []byte("2026-09-09T10:00:00Z [INFO] first\n2026-09-09T10:00:01Z [INFO] second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	initial, err := ReadConfiguredLog(context.Background(), LogSource{ServerID: "server-local-0001", ID: "tail", Label: "Tail", Path: path}, LogReadOptions{MaxEntries: 1})
	if err != nil || len(initial.Entries) != 1 {
		t.Fatalf("initial bounded read failed: %#v err=%v", initial, err)
	}
	tail, err := TailConfiguredLog(context.Background(), LogSource{ServerID: "server-local-0001", ID: "tail", Label: "Tail", Path: path}, LogTailOptions{Cursor: initial.Entries[0].Cursor, MaxEntries: 2, MaxDuration: time.Millisecond})
	if err != nil || len(tail.Entries) != 1 || tail.Entries[0].Text != "second" {
		t.Fatalf("tail cursor was not respected: %#v err=%v", tail, err)
	}
	empty, err := TailConfiguredLog(context.Background(), LogSource{ServerID: "server-local-0001", ID: "tail", Label: "Tail", Path: path}, LogTailOptions{Cursor: tail.Entries[0].Cursor, MaxDuration: time.Millisecond})
	if err != nil || len(empty.Entries) != 0 {
		t.Fatalf("bounded empty tail failed: %#v err=%v", empty, err)
	}
}

func TestTailConfiguredLogKeepsCursorIdentityAcrossAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "append.log")
	if err := os.WriteFile(path, []byte("2026-09-09T10:00:00Z [INFO] first\n2026-09-09T10:00:01Z [INFO] second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	initial, err := ReadConfiguredLog(context.Background(), LogSource{ServerID: "server-local-0001", ID: "append", Label: "Append", Path: path}, LogReadOptions{MaxEntries: 1})
	if err != nil || len(initial.Entries) != 1 {
		t.Fatalf("initial bounded read failed: %#v err=%v", initial, err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("2026-09-09T10:00:02Z [INFO] appended\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	tail, err := TailConfiguredLog(context.Background(), LogSource{ServerID: "server-local-0001", ID: "append", Label: "Append", Path: path}, LogTailOptions{Cursor: initial.Entries[0].Cursor, MaxEntries: 5, MaxDuration: time.Millisecond})
	if err != nil || len(tail.Entries) != 2 || tail.Entries[0].Text != "second" || tail.Entries[1].Text != "appended" {
		t.Fatalf("append was mistaken for rotation and replayed old entries: %#v err=%v", tail, err)
	}
}

func TestTailConfiguredLogDetectsSameInodeRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "copytruncate.log")
	if err := os.WriteFile(path, []byte("2026-09-09T10:00:00Z [INFO] first\n2026-09-09T10:00:01Z [INFO] second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	initial, err := ReadConfiguredLog(context.Background(), LogSource{ServerID: "server-local-0001", ID: "copytruncate", Label: "Copytruncate", Path: path}, LogReadOptions{MaxEntries: 1})
	if err != nil || len(initial.Entries) != 1 {
		t.Fatalf("initial read failed: %#v err=%v", initial, err)
	}
	if err := os.WriteFile(path, []byte("2026-09-09T11:00:00Z [INFO] replacement-one\n2026-09-09T11:00:01Z [INFO] replacement-two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tail, err := TailConfiguredLog(context.Background(), LogSource{ServerID: "server-local-0001", ID: "copytruncate", Label: "Copytruncate", Path: path}, LogTailOptions{Cursor: initial.Entries[0].Cursor, MaxEntries: 2, MaxDuration: time.Millisecond})
	if err != nil || len(tail.Entries) != 2 || !tail.Truncated || tail.Entries[0].Text != "replacement-one" {
		t.Fatalf("same-inode rewrite was not detected: %#v err=%v", tail, err)
	}
}

func TestTailConfiguredLogWaitsForUnterminatedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial.log")
	if err := os.WriteFile(path, []byte("2026-09-09T10:00:00Z [INFO] first\npartial"), 0o600); err != nil {
		t.Fatal(err)
	}
	initial, err := ReadConfiguredLog(context.Background(), LogSource{ServerID: "server-local-0001", ID: "partial", Label: "Partial", Path: path}, LogReadOptions{MaxEntries: 1})
	if err != nil || len(initial.Entries) != 1 {
		t.Fatalf("initial partial read failed: %#v err=%v", initial, err)
	}
	fileDone := make(chan struct{})
	go func() {
		defer close(fileDone)
		time.Sleep(20 * time.Millisecond)
		file, openErr := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
		if openErr != nil {
			return
		}
		_, _ = file.WriteString(" line\n")
		_ = file.Close()
	}()
	tail, err := TailConfiguredLog(context.Background(), LogSource{ServerID: "server-local-0001", ID: "partial", Label: "Partial", Path: path}, LogTailOptions{Cursor: initial.Entries[0].Cursor, MaxEntries: 2, MaxDuration: 150 * time.Millisecond, PollInterval: 5 * time.Millisecond})
	<-fileDone
	if err != nil || len(tail.Entries) != 1 || tail.Entries[0].Text != "partial line" {
		t.Fatalf("unterminated line was split or lost: %#v err=%v", tail, err)
	}
}

func TestJournalAdapterRejectsShellLikeUnits(t *testing.T) {
	if _, err := ReadJournal(context.Background(), LogSource{ServerID: "server-local-0001", ID: "journal"}, JournalOptions{Unit: "$(id)"}); err == nil {
		t.Fatal("journal unit accepted shell syntax")
	}
}

func TestDetectSeverityAndOrderedLogQuery(t *testing.T) {
	cases := []struct {
		text     string
		fallback string
		expected string
	}{
		{"payesh: database is locked (5) (SQLITE_BUSY)", "INFO", "ERROR"},
		{"Main process exited, code=exited, status=1/FAILURE", "INFO", "ERROR"},
		{"Failed with result 'exit-code'.", "INFO", "ERROR"},
		{"ERROR: could not bind socket", "INFO", "ERROR"},
		{"level=error msg=\"timeout\"", "INFO", "ERROR"},
		{"[ERROR] failed to connect", "INFO", "ERROR"},
		{"WARN: High disk I/O wait detected: queue depth 8.2", "INFO", "WARN"},
		{"level=warn msg=\"disk full warning\"", "INFO", "WARN"},
		{"[WARN] memory threshold exceeded", "INFO", "WARN"},
		{"Started payesh-server.service successfully", "INFO", "INFO"},
		{"debug trace log", "DEBUG", "DEBUG"},
	}
	for _, tc := range cases {
		actual := DetectSeverity(tc.text, tc.fallback)
		if actual != tc.expected {
			t.Errorf("DetectSeverity(%q, %q) = %q, expected %q", tc.text, tc.fallback, actual, tc.expected)
		}
	}
}
