package transport

import (
	"sync"
	"time"
)

// connectionReportInterval bounds how often an unchanged connection failure is
// repeated in the agent log during a long outage.
const connectionReportInterval = 15 * time.Minute

// connectionStableAfter is how long a session must stay up before the agent
// reports that the connection recovered.
const connectionStableAfter = 20 * time.Second

// connectionReporter turns the agent's silent reconnect loop into a short,
// rate-limited log: the first failure, a changed failure, a reminder at most
// every connectionReportInterval, and the recovery.
type connectionReporter struct {
	mu         sync.Mutex
	logf       func(format string, args ...any)
	now        func() time.Time
	failures   int
	lastError  string
	lastReport time.Time
}

func (r *connectionReporter) failed(endpoint string, err error) {
	if r.logf == nil || err == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures++
	now := r.clock()
	message := err.Error()
	if message == r.lastError && now.Sub(r.lastReport) < connectionReportInterval {
		return
	}
	if r.failures == 1 {
		r.logf("node transport: connection to %s failed: %s (retrying)", endpoint, message)
	} else {
		r.logf("node transport: connection to %s failed: %s (%d failed attempts, retrying)", endpoint, message, r.failures)
	}
	r.lastError, r.lastReport = message, now
}

func (r *connectionReporter) connected(endpoint string) {
	if r.logf == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failures == 0 {
		return
	}
	r.logf("node transport: connected to %s after %d failed attempts", endpoint, r.failures)
	r.failures, r.lastError, r.lastReport = 0, "", time.Time{}
}

func (r *connectionReporter) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}
