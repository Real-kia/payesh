package transport

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

// A node that cannot reach its hub retries forever. Operators need to see why,
// but a long outage must not write one line per retry.
func TestAgentReportsRepeatedConnectionFailureOnce(t *testing.T) {
	spool, err := OpenSpool(filepath.Join(t.TempDir(), "agent.spool"), MaxSpoolBytes)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var lines []string
	client := &AgentClient{
		URL:      "wss://127.0.0.1:1/node/v1",
		Identity: NodeIdentity{ServerID: "server-log-0001"},
		Spool:    spool,
		Backoff:  ReconnectBackoff{Base: time.Millisecond, Max: time.Millisecond, Jitter: func(d time.Duration) time.Duration { return d }},
		Logf: func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			lines = append(lines, fmt.Sprintf(format, args...))
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := client.Run(ctx, make(chan contracts.SampleBatch)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 1 {
		t.Fatalf("want one report for a repeating failure, got %d: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], "missing certificate or key") || !strings.Contains(lines[0], "127.0.0.1:1") {
		t.Fatalf("report lacks the cause or endpoint: %q", lines[0])
	}
}

func TestConnectionReporterRepeatsAfterIntervalAndReportsRecovery(t *testing.T) {
	var lines []string
	now := time.Unix(0, 0)
	reporter := connectionReporter{logf: func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }, now: func() time.Time { return now }}
	failure := fmt.Errorf("remote error: tls: bad certificate")
	reporter.failed("wss://hub/node/v1", failure)
	now = now.Add(time.Minute)
	reporter.failed("wss://hub/node/v1", failure)
	now = now.Add(connectionReportInterval)
	reporter.failed("wss://hub/node/v1", failure)
	reporter.failed("wss://hub/node/v1", fmt.Errorf("dial tcp: connection refused"))
	reporter.connected("wss://hub/node/v1")
	reporter.connected("wss://hub/node/v1")
	if len(lines) != 4 {
		t.Fatalf("want first, repeated-after-interval, changed and recovered reports, got %d: %q", len(lines), lines)
	}
	if !strings.Contains(lines[1], "3 failed attempts") {
		t.Fatalf("repeat report should count attempts: %q", lines[1])
	}
	if !strings.Contains(lines[3], "connected") {
		t.Fatalf("recovery not reported: %q", lines[3])
	}
}
