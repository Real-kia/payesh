package alerts

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

const (
	maxIncidentContextSources = 16
	maxIncidentContextEntries = 32
	maxIncidentContextBytes   = 64 << 10
)

// incidentForWithContext builds the normal bounded incident snapshot and
// augments it with package-03 observation/log context captured at the event
// time. Context is best effort: an unavailable source must not prevent the
// alert state transition itself from being durably committed.
func (e *Engine) incidentForWithContext(ctx context.Context, rule contracts.AlertRule, observation Observation, state contracts.AlertState, at time.Time, event contracts.AlertHistoryEvent) contracts.IncidentSnapshot {
	incident := incidentFor(rule, observation, state, at, event)
	incident.Evidence = append(incident.Evidence, e.collectIncidentContext(ctx, observation)...)
	return incident
}

func (e *Engine) collectIncidentContext(ctx context.Context, observation Observation) []string {
	if e == nil || e.Store == nil || observation.ServerID == "" || observation.ObservedAt.IsZero() {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// A context read is bounded independently from the request that triggered
	// the alert. This prevents a stalled journal/file source from holding the
	// evaluator mutex or delaying the durable alert transition indefinitely.
	contextWindow, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	evidence := make([]string, 0, 16)
	keys := make([]string, 0, len(observation.Values))
	for key := range observation.Values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := observation.Values[key]
		if len(evidence) >= 32 {
			break
		}
		if !isFinite(value) {
			continue
		}
		evidence = append(evidence, boundIncidentEvidence("metric "+key+"="+strconv.FormatFloat(value, 'g', -1, 64)))
	}
	if len(evidence) >= 32 {
		return evidence
	}
	sources, err := e.Store.QueryLogSourcePage(contextWindow, observation.ServerID, maxIncidentContextSources, "")
	if err != nil {
		return evidence
	}
	usedBytes := 0
	for _, source := range sources.Items {
		if len(evidence) >= maxIncidentContextEntries || contextWindow.Err() != nil {
			break
		}
		from := observation.ObservedAt.UTC().Add(-5 * time.Minute)
		to := observation.ObservedAt.UTC().Add(5 * time.Minute)
		entries := make([]monitoring.LogEntry, 0, maxIncidentContextEntries)
		if source.Path != "" {
			snapshot, readErr := monitoring.ReadConfiguredLogCursor(contextWindow, source, monitoring.LogReadOptions{
				MaxEntries:  maxIncidentContextEntries,
				MaxBytes:    maxIncidentContextBytes,
				MaxLineSize: monitoring.DefaultLogLineSize,
				From:        from,
				To:          to,
				Now:         func() time.Time { return observation.ObservedAt.UTC() },
			}, "")
			if readErr == nil {
				entries = snapshot.Entries
			}
		} else {
			snapshot, readErr := monitoring.ReadJournal(contextWindow, source, monitoring.JournalOptions{
				Unit:       source.ID,
				MaxEntries: maxIncidentContextEntries,
				MaxBytes:   maxIncidentContextBytes,
				Since:      from,
				Until:      to,
				Now:        func() time.Time { return observation.ObservedAt.UTC() },
			})
			if readErr == nil {
				entries = snapshot.Entries
			}
		}
		// A source may be temporarily unavailable at the exact alert instant;
		// retain any previously persisted bounded snapshot as a fallback.
		if len(entries) == 0 {
			page, queryErr := e.Store.QueryLogs(contextWindow, observation.ServerID, source.ID, from, to, maxIncidentContextEntries, "")
			if queryErr != nil {
				continue
			}
			entries = page.Entries
		}
		for _, entry := range entries {
			text := strings.ToValidUTF8(entry.Text, "�")
			item := boundIncidentEvidence(fmt.Sprintf("log[%s] %s: %s", source.ID, entry.Timestamp.UTC().Format(time.RFC3339Nano), text))
			if usedBytes+len(item) > maxIncidentContextBytes {
				return evidence
			}
			evidence = append(evidence, item)
			usedBytes += len(item)
			if len(evidence) >= maxIncidentContextEntries {
				return evidence
			}
		}
	}
	return evidence
}

// boundIncidentEvidence keeps each evidence element within the contract's
// byte limit while preserving valid UTF-8. Alert context is best effort, so a
// hostile or unusually long log line must be shortened rather than making
// the surrounding alert transition fail validation.
func boundIncidentEvidence(value string) string {
	value = strings.ToValidUTF8(value, "�")
	if len(value) <= 4096 {
		return value
	}
	cut := 4096
	for cut > 0 && (value[cut]&0xc0) == 0x80 {
		cut--
	}
	return value[:cut]
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
