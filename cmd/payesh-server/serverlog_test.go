package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestHandshakeLogWriterRateLimitsRepeatedFailures(t *testing.T) {
	var out bytes.Buffer
	now := time.Unix(0, 0)
	writer := newHandshakeLogWriter(&out, func() time.Time { return now })
	line := func(port, message string) {
		if _, err := writer.Write([]byte("2026/10/09 13:23:42 http: TLS handshake error from 192.0.2.7:" + port + ": " + message + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	line("44788", "remote error: tls: bad certificate")
	for minute := 1; minute <= 5; minute++ {
		now = now.Add(time.Minute)
		line("4478"+string(rune('0'+minute)), "remote error: tls: bad certificate")
	}
	line("50000", "EOF")
	if _, err := writer.Write([]byte("2026/10/09 13:30:00 http: panic serving 192.0.2.7:1: boom\n")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(handshakeLogInterval)
	line("44799", "remote error: tls: bad certificate")

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("want first failure, a different failure, the unrelated line and one reminder; got %d:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[0], "bad certificate") || !strings.Contains(lines[1], "EOF") || !strings.Contains(lines[2], "panic serving") {
		t.Fatalf("unexpected lines:\n%s", out.String())
	}
	if !strings.Contains(lines[3], "bad certificate") || !strings.Contains(lines[3], "5 similar") {
		t.Fatalf("reminder should count suppressed repeats: %q", lines[3])
	}
}
