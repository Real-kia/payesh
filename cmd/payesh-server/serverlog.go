package main

import (
	"fmt"
	"io"
	"log"
	"regexp"
	"sync"
	"time"
)

// handshakeLogInterval bounds how often the same TLS handshake failure from
// one host is written to the server log.
const handshakeLogInterval = 15 * time.Minute

var handshakeErrorLine = regexp.MustCompile(`^(.*http: TLS handshake error from )(\[[^\]]+\]|[^\s:]+)(?::\d+)?: (.*?)\n?$`)

// handshakeLogWriter rate-limits net/http's per-connection TLS handshake
// errors. A node with a stale trust anchor or certificate retries every
// minute, and scanners probe the public listeners; without a limit each attempt
// adds a line to the hub log. Other server log lines pass through unchanged.
type handshakeLogWriter struct {
	out  io.Writer
	now  func() time.Time
	mu   sync.Mutex
	seen map[string]*handshakeLogEntry
}

type handshakeLogEntry struct {
	reported   time.Time
	suppressed int
}

func newHandshakeLogWriter(out io.Writer, now func() time.Time) *handshakeLogWriter {
	if now == nil {
		now = time.Now
	}
	return &handshakeLogWriter{out: out, now: now, seen: map[string]*handshakeLogEntry{}}
}

func (w *handshakeLogWriter) Write(p []byte) (int, error) {
	match := handshakeErrorLine.FindSubmatch(p)
	if match == nil {
		return w.out.Write(p)
	}
	key := string(match[2]) + " " + string(match[3])
	now := w.now()
	w.mu.Lock()
	entry := w.seen[key]
	if entry != nil && now.Sub(entry.reported) < handshakeLogInterval {
		entry.suppressed++
		w.mu.Unlock()
		return len(p), nil
	}
	suppressed := 0
	if entry != nil {
		suppressed = entry.suppressed
	}
	if len(w.seen) >= 1024 {
		for host, old := range w.seen {
			if now.Sub(old.reported) >= handshakeLogInterval {
				delete(w.seen, host)
			}
		}
	}
	w.seen[key] = &handshakeLogEntry{reported: now}
	w.mu.Unlock()

	line := string(match[1]) + string(match[2]) + ": " + string(match[3])
	if suppressed > 0 {
		line += fmt.Sprintf(" (%d similar since the last report)", suppressed)
	}
	if _, err := io.WriteString(w.out, line+"\n"); err != nil {
		return 0, err
	}
	return len(p), nil
}

// serverErrorLog is the error log for the dashboard and node listeners.
func serverErrorLog(out io.Writer) *log.Logger {
	return log.New(newHandshakeLogWriter(out, nil), "", log.LstdFlags)
}
