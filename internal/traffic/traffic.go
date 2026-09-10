// Package traffic owns calendar billing semantics and host-side forecasts. It
// deliberately does not inspect kernel interfaces or enforce controls: those
// responsibilities belong to the collector and later bandwidth packages.
package traffic

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Real-kia/payesh/internal/alerts"
	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

const (
	DefaultResetDay = 1
	MinimumForecast = 24 * time.Hour
	RecentForecast  = 7 * 24 * time.Hour
	// DefaultPostProcessRetryInterval is deliberately short enough to recover
	// a queue after a restart or a transient derived-write failure, while still
	// keeping retries low priority relative to live ingestion. Each tick runs
	// one bounded ObserveIngestion pass; it never drains the whole queue in one
	// maintenance turn.
	DefaultPostProcessRetryInterval = 30 * time.Second
	// A single counter point cannot establish a recent rate across an
	// arbitrarily long outage. A day is deliberately generous for hourly
	// collection while still making a 48-hour hole reduce forecast coverage.
	MaxForecastInterval = 24 * time.Hour
)

const (
	// One bounded callback can inspect the entire bounded durable queue once.
	// Persistent blocked-stream exclusions make subsequent callbacks cheap,
	// while this ceiling guarantees a ready tail cannot starve behind more than
	// eight pages of low-sequence deferred streams.
	maxPostProcessRetryRounds = (monitoring.MaxPostProcessQueue + monitoring.MaxPageItems - 1) / monitoring.MaxPageItems
	// Startup recovery is intentionally bounded. A live retry worker, rather
	// than startup, owns any remainder of a large queue.
	maxStartupPostProcessPages = 100
)

var (
	ErrUsageDeferred         = errors.New("traffic usage deferred until predecessor is durable")
	ErrPostProcessPending    = errors.New("post-process queue remains pending")
	ErrAllowanceNotEffective = errors.New("traffic allowance was not effective when sample was accepted")
	errNoTrafficSplit        = errors.New("traffic delta has no period boundary")
	errTrafficSplitBounded   = errors.New("traffic delta spans too many periods")
)

func WithDefaults(allowance contracts.TrafficAllowance) contracts.TrafficAllowance {
	if allowance.ResetDay == 0 {
		allowance.ResetDay = DefaultResetDay
	}
	if allowance.Timezone == "" {
		allowance.Timezone = "UTC"
	}
	if len(allowance.WarningPercentages) == 0 {
		allowance.WarningPercentages = []uint8{80, 90, 100}
	}
	return allowance
}

type PeriodPreview struct {
	Current         contracts.TrafficPeriod `json:"current"`
	Proposed        contracts.TrafficPeriod `json:"proposed"`
	AllowanceEffect string                  `json:"allowance_effect"`
	ScheduleEffect  string                  `json:"schedule_effect"`
	StartsAt        time.Time               `json:"starts_at"`
}

type Manager struct {
	Store *monitoring.Store
	Now   func() time.Time
}

// Processor is the package-05 ingestion seam. It runs only after a sample is
// durably accepted, evaluates resource rules, and applies each configured
// traffic allowance before evaluating its traffic rules.
type Processor struct {
	Store     *monitoring.Store
	Manager   *Manager
	Alerts    *alerts.Engine
	mu        sync.Mutex
	processMu sync.Mutex
	servers   map[contracts.ServerID]struct{}
	// blockedStreams persists fair exclusions across bounded drain calls. It is
	// guarded by processMu and cannot exceed the bounded durable queue.
	blockedStreams map[string]struct{}
}

func NewProcessor(store *monitoring.Store, engine *alerts.Engine) (*Processor, error) {
	manager, err := NewManager(store)
	if err != nil {
		return nil, err
	}
	if engine == nil {
		engine, err = alerts.NewEngine(store)
		if err != nil {
			return nil, err
		}
	}
	return &Processor{Store: store, Manager: manager, Alerts: engine, servers: make(map[contracts.ServerID]struct{}), blockedStreams: make(map[string]struct{})}, nil
}

// ProcessSample is idempotent at the ingestion boundary: callers must pass
// inserted=false for a durable duplicate, preventing the same counter delta
// from being charged again on a retry.
func (p *Processor) ProcessSample(ctx context.Context, sample contracts.MetricSample, previous *contracts.MetricSample, inserted bool) ([]alerts.Evaluation, error) {
	if p == nil || p.Store == nil || p.Manager == nil || p.Alerts == nil {
		return nil, errors.New("traffic processor is unavailable")
	}
	if err := sample.Validate(); err != nil {
		return nil, err
	}
	if !inserted {
		return nil, nil
	}
	p.mu.Lock()
	if p.servers == nil {
		p.servers = make(map[contracts.ServerID]struct{})
	}
	if _, initialized := p.servers[sample.ServerID]; !initialized {
		if err := p.Alerts.EnsureStarterRules(ctx, sample.ServerID); err != nil {
			if !errors.Is(err, monitoring.ErrAlertRuleLimit) {
				p.mu.Unlock()
				return nil, err
			}
			// Starter defaults are optional derived policy. Reaching the honest
			// durable rule cap must not leave an already accepted raw sample
			// permanently wedged in the post-process queue.
		}
		p.servers[sample.ServerID] = struct{}{}
	}
	p.mu.Unlock()
	if sample.Sequence != 0 {
		ready, frontierErr := p.Store.SequenceFrontierReady(ctx, sample.ServerID, sample.CollectorEpoch, sample.Sequence)
		if frontierErr != nil {
			return nil, frontierErr
		}
		if !ready {
			return nil, ErrUsageDeferred
		}
	}
	if previous == nil && sample.Sequence != 0 {
		covered, gapErr := p.Store.CoverageGapCovers(ctx, sample.ServerID, sample.CollectorEpoch, 0, sample.Sequence-1)
		if gapErr != nil {
			return nil, gapErr
		}
		if !covered {
			return nil, ErrUsageDeferred
		}
	}
	if previous != nil && (previous.CollectorEpoch != sample.CollectorEpoch || (previous.Sequence == ^uint64(0) || sample.Sequence != previous.Sequence+1)) {
		if previous.CollectorEpoch != sample.CollectorEpoch {
			return nil, ErrUsageDeferred
		}
		if previous.Sequence == ^uint64(0) {
			return nil, ErrUsageDeferred
		}
		covered, gapErr := p.Store.CoverageGapCovers(ctx, sample.ServerID, sample.CollectorEpoch, previous.Sequence+1, sample.Sequence-1)
		if gapErr != nil {
			return nil, gapErr
		}
		if !covered {
			return nil, ErrUsageDeferred
		}
	}
	allowances, err := p.Store.ListTrafficAllowances(ctx, sample.ServerID)
	if err != nil {
		return nil, err
	}
	results := make([]alerts.Evaluation, 0)
	for _, allowance := range allowances {
		if err := p.Alerts.EnsureTrafficRules(ctx, sample.ServerID, allowance); err != nil {
			if !errors.Is(err, monitoring.ErrAlertRuleLimit) {
				return nil, err
			}
			// Usage accounting and existing-rule evaluation remain valid when
			// there is no capacity for another generated warning rule.
		}
		// The first accepted point establishes a baseline; it is not itself a
		// missing interval. A later non-contiguous point is marked uncertain.
		period, usageErr := p.Manager.AddUsageFromStoredAllowance(ctx, sample, previous, allowance)
		if errors.Is(usageErr, ErrAllowanceNotEffective) {
			continue
		}
		if usageErr != nil {
			return nil, usageErr
		}
		trafficResults, evalErr := p.Alerts.EvaluateTraffic(ctx, sample.ServerID, period, sample.ObservedAt)
		if evalErr != nil {
			return nil, evalErr
		}
		results = append(results, trafficResults...)
	}
	coverage := 1.0
	if sample.TimestampUncertainty != "" && sample.TimestampUncertainty != "none" {
		coverage = 0.5
	}
	if previous != nil && (sample.CollectorEpoch != previous.CollectorEpoch || (previous.Sequence != ^uint64(0) && sample.Sequence != previous.Sequence+1)) {
		coverage = 0.5
	}
	if previous == nil && sample.Sequence != 0 {
		coverage = 0.5
	}
	metricResults, err := p.Alerts.EvaluateSample(ctx, sample, coverage)
	if err != nil {
		return nil, err
	}
	results = append(results, metricResults...)
	return results, nil
}

// ObserveSamples consumes the internal post-commit sample list emitted by the
// hub/store ingestion path. The durable duplicate filter ensures retries do
// not charge an already accepted sequence a second time.
func (p *Processor) ObserveSamples(ctx context.Context, samples []contracts.MetricSample) ([]alerts.Evaluation, error) {
	results := make([]alerts.Evaluation, 0)
	for _, sample := range samples {
		previous, found, err := p.Store.PreviousMetricSample(ctx, sample.ServerID, sample.CollectorEpoch, sample.Sequence)
		if err != nil {
			return nil, err
		}
		var previousPtr *contracts.MetricSample
		if found {
			// Pass the nearest prior point even across a sequence gap. CounterDelta
			// must record that interval as uncertain rather than treating the
			// current point as a fresh baseline and silently losing the gap.
			previousPtr = &previous
		}
		evaluations, err := p.ProcessSample(ctx, sample, previousPtr, true)
		if err != nil {
			return nil, err
		}
		results = append(results, evaluations...)
	}
	return results, nil
}

// ObserveIngestion adapts the processor to transport.Hub's post-commit
// observer callback. The returned evaluations are durable; the callback only
// needs to report processing failure to the transport layer.
func (p *Processor) ObserveIngestion(ctx context.Context, samples []contracts.MetricSample) error {
	p.processMu.Lock()
	defer p.processMu.Unlock()
	if p.blockedStreams == nil {
		p.blockedStreams = make(map[string]struct{})
	}
	if samples != nil && len(samples) == 0 {
		// A non-nil empty callback represents newly committed coverage gaps.
		// Any blocked stream may now have an explicit predecessor range.
		clear(p.blockedStreams)
	} else {
		for _, sample := range samples {
			delete(p.blockedStreams, postProcessStreamIdentity(sample))
		}
	}
	// Drain the durable queue, not just the callback payload. The latter is
	// only an optimization for the just-committed batch and is unavailable
	// after a process restart.
	var pending []contracts.MetricSample
	var err error
	if len(p.blockedStreams) == 0 {
		pending, err = p.Store.ListPendingPostProcessSamples(ctx, monitoring.MaxPageItems)
	} else {
		pending, err = p.Store.ListPendingPostProcessSamplesSkipping(ctx, monitoring.MaxPageItems, p.blockedStreams)
	}
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		if len(p.blockedStreams) > 0 {
			// No non-blocked work remains. Drop the bounded exclusion cache so the
			// next retry rechecks streams whose predecessor may have arrived without
			// an observer callback (for example during startup recovery).
			clear(p.blockedStreams)
			return ErrUsageDeferred
		}
		return nil
	}
	deferred := false
	pageFull := len(pending) == monitoring.MaxPageItems
	processPage := func(page []contracts.MetricSample) error {
		for _, sample := range page {
			if _, err := p.ObserveSamples(ctx, []contracts.MetricSample{sample}); err != nil {
				if errors.Is(err, ErrUsageDeferred) {
					deferred = true
					p.blockedStreams[postProcessStreamIdentity(sample)] = struct{}{}
					continue
				}
				return err
			}
			if err := p.Store.AcknowledgePostProcessSample(ctx, sample); err != nil {
				return err
			}
			delete(p.blockedStreams, postProcessStreamIdentity(sample))
		}
		return nil
	}
	if err := processPage(pending); err != nil {
		return err
	}
	// A page can be full of samples from streams waiting for predecessors.
	// Re-query with all blocked identities excluded, for a bounded number of
	// rounds, so ready streams are not starved behind a chain of blocked ones.
	for round := 0; len(p.blockedStreams) > 0 && round < maxPostProcessRetryRounds; round++ {
		ready, err := p.Store.ListPendingPostProcessSamplesSkipping(ctx, monitoring.MaxPageItems, p.blockedStreams)
		if err != nil {
			return err
		}
		if len(ready) == 0 {
			pageFull = false
			break
		}
		pageFull = len(ready) == monitoring.MaxPageItems
		beforeBlocked := len(p.blockedStreams)
		if err := processPage(ready); err != nil {
			return err
		}
		if len(p.blockedStreams) == beforeBlocked && !pageFull {
			break
		}
	}
	if deferred {
		return ErrUsageDeferred
	}
	if pageFull {
		return ErrPostProcessPending
	}
	return nil
}

func postProcessStreamIdentity(sample contracts.MetricSample) string {
	return string(sample.ServerID) + "\x00" + string(sample.CollectorEpoch)
}

// DrainPendingPostProcess is used during startup/recovery. It may consume
// multiple bounded callback pages, while a deferred out-of-order sample is
// left durable for a later predecessor arrival. A deferred or still-pending
// queue is returned to the caller explicitly; startup must not report a clean
// drain unless all work was acknowledged. Long-lived processes should start
// StartPostProcessRetry after opening the store so this status does not make a
// blocked stream permanent.
func (p *Processor) DrainPendingPostProcess(ctx context.Context) error {
	for page := 0; page < maxStartupPostProcessPages; page++ {
		err := p.ObserveIngestion(ctx, nil)
		if errors.Is(err, ErrUsageDeferred) {
			return ErrUsageDeferred
		}
		if errors.Is(err, ErrPostProcessPending) {
			continue
		}
		return err
	}
	// A bounded startup pass must not claim that a large queue was drained. The
	// retry worker can continue from the durable queue without asking a node to
	// resend any samples.
	return ErrPostProcessPending
}

// StartPostProcessRetry starts the durable post-process retry worker and
// returns a channel that closes when the worker exits. The worker performs an
// immediate bounded pass, then retries once per interval until ctx is done.
// ErrUsageDeferred and ErrPostProcessPending are expected queue states, not
// worker failures: a missing predecessor may arrive later, and a full page is
// continued on the next tick. Other errors are passed to report and retried on
// subsequent ticks. Processing remains serialized by processMu, so a worker
// cannot race a post-commit callback or charge a sample twice.
func (p *Processor) StartPostProcessRetry(ctx context.Context, interval time.Duration, report func(error)) <-chan struct{} {
	done := make(chan struct{})
	if ctx == nil {
		close(done)
		return done
	}
	if interval <= 0 {
		interval = DefaultPostProcessRetryInterval
	}
	go func() {
		defer close(done)
		attempt := func() {
			if ctx.Err() != nil {
				return
			}
			var err error
			if p == nil || p.Store == nil || p.Manager == nil || p.Alerts == nil {
				err = errors.New("traffic processor is unavailable")
			} else {
				err = p.ObserveIngestion(ctx, nil)
			}
			if err == nil || errors.Is(err, ErrUsageDeferred) || errors.Is(err, ErrPostProcessPending) || ctx.Err() != nil {
				return
			}
			if report != nil {
				report(err)
			}
		}
		attempt()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				attempt()
			}
		}
	}()
	return done
}

func NewManager(store *monitoring.Store) (*Manager, error) {
	if store == nil {
		return nil, errors.New("monitoring store is required")
	}
	return &Manager{Store: store, Now: func() time.Time { return time.Now().UTC() }}, nil
}

// PeriodFor computes explicit calendar boundaries in the allowance timezone.
// It never assumes a fixed number of days; reset days 29..31 are clamped to
// the final day of a shorter month, including February in leap years.
func PeriodFor(at time.Time, allowance contracts.TrafficAllowance) (contracts.TrafficPeriod, error) {
	allowance = WithDefaults(allowance)
	if err := allowance.Validate(); err != nil {
		return contracts.TrafficPeriod{}, err
	}
	if at.IsZero() {
		return contracts.TrafficPeriod{}, errors.New("period timestamp is required")
	}
	location, err := time.LoadLocation(allowance.Timezone)
	if err != nil {
		return contracts.TrafficPeriod{}, err
	}
	local := at.In(location)
	start := calendarBoundary(local.Year(), local.Month(), allowance.ResetDay, location)
	if local.Before(start) {
		previousYear, previousMonth := local.Year(), local.Month()-1
		if previousMonth < 1 {
			previousYear--
			previousMonth = 12
		}
		start = calendarBoundary(previousYear, previousMonth, allowance.ResetDay, location)
	}
	nextYear, nextMonth := start.In(location).Year(), start.In(location).Month()+1
	if nextMonth > 12 {
		nextYear++
		nextMonth = 1
	}
	next := calendarBoundary(nextYear, nextMonth, allowance.ResetDay, location)
	return contracts.TrafficPeriod{Scope: allowance.Scope, Interfaces: append([]string(nil), allowance.Interfaces...), From: start.UTC(), To: next.UTC(), Timezone: allowance.Timezone, AllowanceBytes: allowance.AllowanceBytes, Direction: allowance.Direction, Continuity: "complete"}, nil
}

func calendarBoundary(year int, month time.Month, resetDay uint8, location *time.Location) time.Time {
	day := int(resetDay)
	last := time.Date(year, month+1, 0, 0, 0, 0, 0, location).Day()
	if day > last {
		day = last
	}
	return time.Date(year, month, day, 0, 0, 0, 0, location)
}

// PreviewChange makes the non-obvious schedule behavior explicit: allowance
// size changes apply to the active period, while schedule/timezone changes
// wait for the next existing boundary unless the owner requests a new period.
func PreviewChange(current contracts.TrafficPeriod, proposed contracts.TrafficAllowance, at time.Time, startNewPeriod bool) (PeriodPreview, error) {
	if err := current.Validate(); err != nil {
		return PeriodPreview{}, err
	}
	proposed = WithDefaults(proposed)
	if err := proposed.Validate(); err != nil {
		return PeriodPreview{}, err
	}
	if at.IsZero() {
		return PeriodPreview{}, errors.New("preview timestamp is required")
	}
	active := current
	active.AllowanceBytes = proposed.AllowanceBytes
	preview := PeriodPreview{Current: current, Proposed: active, AllowanceEffect: "active_period_immediately", ScheduleEffect: "next_existing_boundary", StartsAt: current.From}
	proposedPeriod, err := PeriodFor(at, proposed)
	if err != nil {
		return PeriodPreview{}, err
	}
	// Comparing the explicit boundary (rather than adding one nominal month)
	// handles reset-day clamping and DST transitions correctly.
	scheduleChanged := current.From.UTC() != proposedPeriod.From.UTC() || current.To.UTC() != proposedPeriod.To.UTC() || current.Timezone != proposed.Timezone || current.Scope != proposed.Scope || current.Direction != proposed.Direction || !sameInterfaces(current.Interfaces, proposed.Interfaces)
	if startNewPeriod {
		fresh, err := newPeriodFor(at, proposed)
		if err != nil {
			return PeriodPreview{}, err
		}
		preview.Proposed = fresh
		preview.ScheduleEffect = "explicit_new_period"
		preview.StartsAt = fresh.From
	} else if scheduleChanged {
		// Keep the currently active period identity in Current and show the
		// first concrete proposed period beginning at the existing boundary.
		// This is more useful than returning the old period with only a changed
		// timezone, and remains correct for clamped reset days and DST.
		preview.Proposed = periodStartingAt(current.To, proposed)
		preview.StartsAt = current.To
	}
	return preview, nil
}

// AddUsage persists a selected allowance and atomically updates its current
// calendar period. The period key is the server/scope/start/direction tuple,
// so process restarts cannot silently grant a fresh allowance.
func (m *Manager) AddUsage(ctx context.Context, serverID contracts.ServerID, allowance contracts.TrafficAllowance, at time.Time, delta uint64, continuity string) (contracts.TrafficPeriod, error) {
	return m.ApplyAllowance(ctx, serverID, allowance, at, delta, continuity, false)
}

// AddUsageFromStoredAllowance is the ingestion-only variant of AddUsage. The
// processor passes a snapshot obtained while enumerating configured
// allowances, so this method re-reads the authoritative row and never writes
// that potentially stale snapshot back over a newer configuration edit.
func (m *Manager) AddUsageFromStoredAllowance(ctx context.Context, sample contracts.MetricSample, previous *contracts.MetricSample, allowance contracts.TrafficAllowance) (contracts.TrafficPeriod, error) {
	if err := sample.Validate(); err != nil {
		return contracts.TrafficPeriod{}, err
	}
	var result contracts.TrafficPeriod
	err := m.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		authoritative, found, err := txGetTrafficAllowance(ctx, tx, sample.ServerID, allowance.Scope, allowance.Direction)
		if err != nil {
			return err
		}
		if found {
			allowance = authoritative
		}
		// Configuration is selected by durable acceptance time, not by when the
		// post-process worker happens to drain this sample. This prevents queue
		// delay or restart from retroactively applying a newly created/edited
		// interface or direction.
		deltaAllowance, effective, effectiveErr := txGetTrafficAllowanceAt(ctx, tx, sample.ServerID, allowance.Scope, allowance.Direction, sample.ReceivedAt.UTC())
		if effectiveErr != nil {
			return effectiveErr
		}
		if !effective {
			if found {
				return ErrAllowanceNotEffective
			}
			// Direct/library ingestion may bootstrap the very first allowance
			// from the supplied snapshot. Once a durable allowance exists, only
			// its acceptance-time version is authoritative.
			deltaAllowance = WithDefaults(allowance)
			if err := deltaAllowance.Validate(); err != nil {
				return err
			}
		}
		pending, hasPending, pendingErr := txGetTrafficAllowanceChange(ctx, tx, sample.ServerID, allowance.Scope, allowance.Direction)
		if pendingErr != nil {
			return pendingErr
		} else if hasPending && !sample.ObservedAt.After(pending.EffectiveAt) {
			// Interface selection follows the same boundary hand-off as reset
			// day/timezone. The bytes observed before the boundary must use the
			// previous interface set, even though the proposed config is already
			// durable in traffic_allowances.
			deltaAllowance = applyOldSchedule(pending.Previous, allowance)
		}
		// A durable explicit period can overlap the old calendar period. Use the
		// period active at the observation instant when selecting counters; an
		// allowance row may already contain a newer interface set while a delayed
		// sample still belongs to the old period. If the observation lands exactly
		// on the new period's start, the interval ending there is still owned by
		// the old end-exclusive period.
		if active, activeFound, activeErr := txGetActiveTrafficPeriod(ctx, tx, sample.ServerID, allowance.Scope, allowance.Direction, sample.ObservedAt.UTC()); activeErr != nil {
			return activeErr
		} else if activeFound {
			deltaAllowance.Interfaces = append([]string(nil), active.Interfaces...)
			if previous != nil && sample.ObservedAt.Equal(active.From) && sample.ObservedAt.After(previous.ObservedAt) {
				if prior, priorFound, priorErr := txGetActiveTrafficPeriod(ctx, tx, sample.ServerID, allowance.Scope, allowance.Direction, previous.ObservedAt.UTC()); priorErr != nil {
					return priorErr
				} else if priorFound && prior.From.Before(active.From) {
					deltaAllowance.Interfaces = append([]string(nil), prior.Interfaces...)
				}
			}
		}
		delta, continuity := uint64(0), "complete"
		interfaceTransition := false
		if previous == nil {
			if sample.Sequence != 0 {
				// Epoch streams start at zero. A first point at a later sequence is a
				// baseline with an unreported prefix, not a complete interval.
				continuity = "uncertain"
			} else {
				// Sequence zero proves only within-epoch continuity. If another epoch
				// already exists for this server, this is a reboot boundary and the
				// unobserved cross-epoch interval must make the billing period
				// unforecastable instead of looking like a clean first sample.
				epochBoundary, boundaryErr := monitoring.CollectorEpochHasPredecessorTx(ctx, tx, sample.ServerID, sample.CollectorEpoch)
				if boundaryErr != nil {
					return boundaryErr
				}
				if epochBoundary {
					continuity = "uncertain"
				}
			}
		}
		if previous != nil {
			delta, continuity, err = CounterDelta(*previous, sample, deltaAllowance)
			if err != nil {
				return err
			}
			if continuity == "uncertain" && (previous.CollectorEpoch != sample.CollectorEpoch || previous.Sequence == ^uint64(0) || previous.Sequence+1 != sample.Sequence) {
				if previous.CollectorEpoch != sample.CollectorEpoch || previous.Sequence == ^uint64(0) || sample.Sequence == 0 {
					return ErrUsageDeferred
				}
				from := previous.Sequence + 1
				to := sample.Sequence - 1
				covered, gapErr := txGapCovers(ctx, tx, sample.ServerID, sample.CollectorEpoch, from, to)
				if gapErr != nil {
					return gapErr
				}
				if !covered {
					return ErrUsageDeferred
				}
			}
			if sample.ObservedAt.After(previous.ObservedAt) {
				interfaceTransition, err = hasInterfaceTransitionTx(ctx, tx, sample.ServerID, allowance.Scope, allowance.Direction, pending, hasPending, allowance, previous.ObservedAt, sample.ObservedAt)
				if err != nil {
					return err
				}
				if interfaceTransition {
					// A counter delta across an interface-set transition cannot be
					// attributed to either side. Never feed it to the proportional
					// period splitter, even when both interface counter keys happen
					// to be present in both samples.
					delta = 0
					continuity = "uncertain"
				}
			}
		} else if sample.Sequence != 0 {
			covered, gapErr := txGapCovers(ctx, tx, sample.ServerID, sample.CollectorEpoch, 0, sample.Sequence-1)
			if gapErr != nil {
				return gapErr
			}
			if !covered {
				return ErrUsageDeferred
			}
		}
		alreadyApplied, err := monitoring.TrafficUsageAlreadyAppliedTx(ctx, tx, sample.ServerID, sample.CollectorEpoch, sample.Sequence, allowance.Scope, allowance.Direction)
		if err != nil {
			return err
		}
		if !alreadyApplied {
			inserted, insertErr := tx.ExecContext(ctx, `INSERT INTO traffic_usage_ledger(server_id,collector_epoch,sequence,scope,direction) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, string(sample.ServerID), string(sample.CollectorEpoch), strconv.FormatUint(sample.Sequence, 10), allowance.Scope, allowance.Direction)
			if insertErr != nil {
				return insertErr
			}
			rows, rowsErr := inserted.RowsAffected()
			if rowsErr != nil {
				return rowsErr
			}
			alreadyApplied = rows == 0
		}
		if alreadyApplied {
			active, activeFound, activeErr := txGetActiveTrafficPeriod(ctx, tx, sample.ServerID, allowance.Scope, allowance.Direction, sample.ObservedAt.UTC())
			if activeErr != nil {
				return activeErr
			}
			if activeFound {
				result = active
				return nil
			}
			result, err = PeriodFor(sample.ObservedAt, deltaAllowance)
			return err
		}
		if interfaceTransition {
			result, err = m.applyInterfaceTransitionUsageTx(ctx, tx, sample.ServerID, deltaAllowance, !found, previous.ObservedAt, sample.ObservedAt)
		} else if continuity == "complete" && previous != nil && sample.ObservedAt.After(previous.ObservedAt) {
			result, err = m.applySplitUsageTx(ctx, tx, sample.ServerID, deltaAllowance, pending, hasPending, !found, previous.ObservedAt, sample.ObservedAt, delta)
			if errors.Is(err, errTrafficSplitBounded) {
				// A very sparse/delayed stream can span more periods than the
				// bounded splitter is allowed to materialize. Preserve the
				// interval as uncertain and acknowledge the sample instead of
				// charging the entire delta to whichever period is current.
				result, err = m.applyInterfaceTransitionUsageTx(ctx, tx, sample.ServerID, deltaAllowance, !found, previous.ObservedAt, sample.ObservedAt)
			} else if errors.Is(err, errNoTrafficSplit) {
				err = nil
				result, err = m.applyAllowanceTx(ctx, tx, sample.ServerID, deltaAllowance, sample.ObservedAt, delta, continuity, false, !found)
			}
		} else {
			result, err = m.applyAllowanceTx(ctx, tx, sample.ServerID, deltaAllowance, sample.ObservedAt, delta, continuity, false, !found)
		}
		return err
	})
	return result, err
}

// ApplyAllowance persists a new configuration and updates the period that is
// active at `at`. Schedule changes take effect at the next boundary by default;
// callers must explicitly request startNewPeriod to make a new identity now.
func (m *Manager) ApplyAllowance(ctx context.Context, serverID contracts.ServerID, allowance contracts.TrafficAllowance, at time.Time, delta uint64, continuity string, startNewPeriod bool) (contracts.TrafficPeriod, error) {
	allowance = WithDefaults(allowance)
	var result contracts.TrafficPeriod
	err := m.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = m.applyAllowanceTx(ctx, tx, serverID, allowance, at, delta, continuity, startNewPeriod, true)
		return err
	})
	return result, err
}

func (m *Manager) applyAllowance(ctx context.Context, serverID contracts.ServerID, allowance contracts.TrafficAllowance, at time.Time, delta uint64, continuity string, startNewPeriod, persistAllowance bool) (contracts.TrafficPeriod, error) {
	allowance = WithDefaults(allowance)
	var result contracts.TrafficPeriod
	err := m.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = m.applyAllowanceTx(ctx, tx, serverID, allowance, at, delta, continuity, startNewPeriod, persistAllowance)
		return err
	})
	return result, err
}

func applyOldSchedule(previous, proposed contracts.TrafficAllowance) contracts.TrafficAllowance {
	proposed.ResetDay = previous.ResetDay
	proposed.Timezone = previous.Timezone
	proposed.Interfaces = append([]string(nil), previous.Interfaces...)
	return proposed
}

// hasInterfaceTransitionTx reports whether a counter interval crosses a
// durable period boundary that changes the selected interface set. The
// counter values do not carry a transition marker, so even valid counters on
// both sides cannot establish how the delta should be assigned.
//
// The bounds are intentionally strict: an interval ending exactly at a
// boundary belongs to the preceding period, matching the period's end
// exclusive semantics. A later observation that is strictly after the
// boundary is the first one that needs uncertainty.
func hasInterfaceTransitionTx(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, scope, direction string, pending monitoring.TrafficAllowanceChange, hasPending bool, allowance contracts.TrafficAllowance, start, end time.Time) (bool, error) {
	if !end.After(start) {
		return false, nil
	}
	if hasPending && start.Before(pending.EffectiveAt.UTC()) && end.After(pending.EffectiveAt.UTC()) && !sameInterfaces(pending.Previous.Interfaces, allowance.Interfaces) {
		return true, nil
	}
	previous, found, err := txGetActiveTrafficPeriod(ctx, tx, serverID, scope, direction, start.UTC())
	if err != nil || !found {
		return false, err
	}
	periods, err := txListTrafficPeriodsIntersecting(ctx, tx, serverID, scope, direction, start, end)
	if errors.Is(err, errTrafficSplitBounded) {
		// More than the bounded number of rows means we cannot prove that no
		// interface transition lies in the interval. Treat it as uncertain;
		// the caller will acknowledge the sample after marking the bounded
		// touched rows and the current period.
		return true, nil
	}
	if err != nil {
		return false, err
	}
	for _, period := range periods {
		if period.From.After(start.UTC()) && period.From.Before(end.UTC()) && !sameInterfaces(previous.Interfaces, period.Interfaces) {
			return true, nil
		}
	}
	// A calendar period may not have been materialized yet. Derive the period
	// at the endpoint as a bounded fallback so a delayed first post-cutover
	// sample still cannot fabricate attribution.
	proposed, err := PeriodFor(end, allowance)
	if err != nil {
		return false, err
	}
	if proposed.From.After(start.UTC()) && proposed.From.Before(end.UTC()) && !sameInterfaces(previous.Interfaces, proposed.Interfaces) {
		return true, nil
	}
	return false, nil
}

// applyInterfaceTransitionUsageTx records the interval as uncertain without
// charging any bytes. Every durable period touched by the interval is marked
// uncertain, including both sides of an overlapping explicit period, so an
// old period is not presented as complete after an unattributable hand-off.
func (m *Manager) applyInterfaceTransitionUsageTx(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, allowance contracts.TrafficAllowance, persistAllowance bool, start, end time.Time) (contracts.TrafficPeriod, error) {
	periods, err := txListTrafficPeriodsIntersecting(ctx, tx, serverID, allowance.Scope, allowance.Direction, start, end)
	if errors.Is(err, errTrafficSplitBounded) {
		// Mark every affected durable row with one bounded SQL update. The
		// materialized slice is still limited for any per-row bookkeeping, but
		// rows beyond that cap must not remain falsely complete.
		if markErr := txMarkTrafficPeriodsUncertain(ctx, tx, serverID, allowance.Scope, allowance.Direction, start, end); markErr != nil {
			return contracts.TrafficPeriod{}, markErr
		}
		err = nil
	}
	if err != nil {
		return contracts.TrafficPeriod{}, err
	}
	for _, period := range periods {
		if _, err := txAddTrafficUsage(ctx, tx, serverID, period, 0, "uncertain"); err != nil {
			return contracts.TrafficPeriod{}, err
		}
	}
	return m.applyAllowanceTx(ctx, tx, serverID, allowance, end, 0, "uncertain", false, persistAllowance)
}

func txMarkTrafficPeriodsUncertain(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, scope, direction string, start, end time.Time) error {
	if !end.After(start) {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE traffic_periods SET continuity='uncertain' WHERE server_id=? AND scope=? AND direction=? AND period_end > ? AND period_start < ?`, string(serverID), scope, direction, monitoring.FormatPersistedTime(start), monitoring.FormatPersistedTime(end))
	return err
}

// applySplitUsageTx attributes a complete counter interval to every calendar
// (or scheduled) period it crosses. It is deliberately bounded: a collector
// interval spanning more than 64 periods is handed to the uncertain path by
// the caller rather than creating unbounded transaction work.
func (m *Manager) applySplitUsageTx(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, allowance contracts.TrafficAllowance, pending monitoring.TrafficAllowanceChange, hasPending, persistAllowance bool, start, end time.Time, delta uint64) (contracts.TrafficPeriod, error) {
	if !end.After(start) {
		return contracts.TrafficPeriod{}, errNoTrafficSplit
	}
	start, end = start.UTC(), end.UTC()
	if hasPending && start.Before(pending.EffectiveAt.UTC()) && end.After(pending.EffectiveAt.UTC()) && !sameInterfaces(pending.Previous.Interfaces, allowance.Interfaces) {
		// Counter values do not identify how bytes were divided between old and
		// new selected interfaces. Preserve the sample as uncertain instead of
		// charging an invented split.
		return m.applyAllowanceTx(ctx, tx, serverID, allowance, end, 0, "uncertain", false, persistAllowance)
	}
	type segment struct {
		start, end time.Time
		allowance  contracts.TrafficAllowance
		period     contracts.TrafficPeriod
	}
	segments := make([]segment, 0, 4)
	cursor := start
	reachedBoundaryAtEnd := false
	for cursor.Before(end) {
		segmentAllowance := allowance
		if hasPending && cursor.Before(pending.EffectiveAt.UTC()) {
			segmentAllowance = applyOldSchedule(pending.Previous, allowance)
		}
		var period contracts.TrafficPeriod
		var found bool
		var err error
		// A pending schedule's previous period remains authoritative only up to
		// its effective instant. After that point derive the proposed schedule;
		// otherwise an old explicit period can incorrectly absorb post-cutover
		// usage merely because its stored end is later than the transition.
		if !hasPending || cursor.Before(pending.EffectiveAt.UTC()) {
			period, found, err = txGetActiveTrafficPeriod(ctx, tx, serverID, segmentAllowance.Scope, segmentAllowance.Direction, cursor)
		}
		if err != nil {
			return contracts.TrafficPeriod{}, err
		}
		if !found {
			period, err = PeriodFor(cursor, segmentAllowance)
			if err != nil {
				return contracts.TrafficPeriod{}, err
			}
		}
		boundary := period.To.UTC()
		if hasPending && pending.EffectiveAt.UTC().After(cursor) && pending.EffectiveAt.UTC().Before(boundary) {
			boundary = pending.EffectiveAt.UTC()
		}
		if nextStart, nextFound, nextErr := txNextTrafficPeriodStart(ctx, tx, serverID, segmentAllowance.Scope, segmentAllowance.Direction, cursor, end); nextErr != nil {
			return contracts.TrafficPeriod{}, nextErr
		} else if nextFound && nextStart.Before(boundary) {
			boundary = nextStart
		}
		if !boundary.After(cursor) {
			return contracts.TrafficPeriod{}, errors.New("traffic period boundary did not advance")
		}
		if boundary.Equal(end) {
			reachedBoundaryAtEnd = true
		}
		if boundary.After(end) {
			boundary = end
		}
		segments = append(segments, segment{start: cursor, end: boundary, allowance: segmentAllowance, period: period})
		if len(segments) > 64 {
			return contracts.TrafficPeriod{}, errTrafficSplitBounded
		}
		cursor = boundary
	}
	if len(segments) < 2 && !reachedBoundaryAtEnd {
		return contracts.TrafficPeriod{}, errNoTrafficSplit
	}
	if persistAllowance {
		if err := txUpsertTrafficAllowance(ctx, tx, serverID, allowance, end); err != nil {
			return contracts.TrafficPeriod{}, err
		}
	}
	if hasPending && !end.Before(pending.EffectiveAt.UTC()) {
		if _, err := tx.ExecContext(ctx, `DELETE FROM traffic_allowance_changes WHERE server_id=? AND scope=? AND direction=?`, string(serverID), allowance.Scope, allowance.Direction); err != nil {
			return contracts.TrafficPeriod{}, err
		}
	}
	total := end.Sub(start)
	if total <= 0 {
		return contracts.TrafficPeriod{}, errNoTrafficSplit
	}
	allocated := uint64(0)
	var result contracts.TrafficPeriod
	var err error
	for i, item := range segments {
		part := uint64(0)
		if i == len(segments)-1 {
			part = delta - allocated
		} else {
			// Compute floor(delta * elapsed / total) with arbitrary precision;
			// elapsed-duration multiplication otherwise overflows for uint64
			// counters even on perfectly ordinary long-running streams.
			numerator := new(big.Int).Mul(new(big.Int).SetUint64(delta), big.NewInt(item.end.Sub(start).Nanoseconds()))
			quotient := new(big.Int).Quo(numerator, big.NewInt(total.Nanoseconds()))
			if !quotient.IsUint64() {
				return contracts.TrafficPeriod{}, errors.New("traffic split exceeds uint64")
			}
			cumulative := quotient.Uint64()
			if cumulative < allocated {
				return contracts.TrafficPeriod{}, errors.New("traffic split allocation regressed")
			}
			part = cumulative - allocated
		}
		result, err = txAddTrafficUsage(ctx, tx, serverID, item.period, part, "complete")
		if err != nil {
			return contracts.TrafficPeriod{}, err
		}
		allocated += part
	}
	return result, nil
}

// newPeriodFor starts an explicitly requested period at the owner's chosen
// instant. Its first end is still the next concrete calendar boundary under
// the proposed schedule; subsequent periods are produced by PeriodFor.
func newPeriodFor(at time.Time, allowance contracts.TrafficAllowance) (contracts.TrafficPeriod, error) {
	period, err := PeriodFor(at, allowance)
	if err != nil {
		return contracts.TrafficPeriod{}, err
	}
	if at.UTC().After(period.From.UTC()) {
		period.From = at.UTC()
		// PeriodFor already computed the first concrete boundary after `at`.
		// Reusing it is important when an explicit period starts before this
		// month's reset day (for example day 10 with reset day 15).
	}
	return period, nil
}

func periodStartingAt(start time.Time, allowance contracts.TrafficAllowance) contracts.TrafficPeriod {
	// PeriodFor(start) returns the next boundary after start, including when
	// start lands exactly on a reset boundary. The transition period therefore
	// ends at the first proposed boundary and cannot silently span two periods.
	calculated, err := PeriodFor(start, allowance)
	if err != nil {
		return contracts.TrafficPeriod{Scope: allowance.Scope, Interfaces: append([]string(nil), allowance.Interfaces...), From: start.UTC(), To: start.UTC(), Timezone: allowance.Timezone, AllowanceBytes: allowance.AllowanceBytes, Direction: allowance.Direction, Continuity: "complete"}
	}
	calculated.From = start.UTC()
	calculated.Interfaces = append([]string(nil), allowance.Interfaces...)
	return calculated
}

func sameInterfaces(current, proposed []string) bool {
	if len(current) != len(proposed) {
		return false
	}
	seen := make(map[string]struct{}, len(current))
	for _, iface := range current {
		seen[iface] = struct{}{}
	}
	for _, iface := range proposed {
		if _, ok := seen[iface]; !ok {
			return false
		}
	}
	return true
}

func allowanceScheduleChanged(previous, proposed contracts.TrafficAllowance) bool {
	if previous.ResetDay != proposed.ResetDay || previous.Timezone != proposed.Timezone || previous.Scope != proposed.Scope || previous.Direction != proposed.Direction {
		return true
	}
	return !sameInterfaces(previous.Interfaces, proposed.Interfaces)
}

// applyAllowanceTx is the transaction-backed form used by the HTTP adapter.
// It mirrors ApplyAllowance's calendar policy while keeping the allowance,
// period total, revision, and idempotency record in one SQLite transaction.
func (m *Manager) applyAllowanceTx(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, allowance contracts.TrafficAllowance, at time.Time, delta uint64, continuity string, startNewPeriod, persistAllowance bool) (contracts.TrafficPeriod, error) {
	if err := allowance.Validate(); err != nil {
		return contracts.TrafficPeriod{}, err
	}
	if at.IsZero() {
		return contracts.TrafficPeriod{}, errors.New("usage timestamp is required")
	}
	if continuity != "complete" && continuity != "gap" && continuity != "uncertain" {
		return contracts.TrafficPeriod{}, errors.New("invalid traffic continuity")
	}
	if !persistAllowance && !startNewPeriod {
		// Ingestion already resolved this snapshot by the sample's durable
		// acceptance time. Do not re-run current configuration reconciliation:
		// a delayed historical sample must materialize the accepted policy, not
		// whichever allowance happens to be current when the queue drains.
		period, err := PeriodFor(at, allowance)
		if err != nil {
			return contracts.TrafficPeriod{}, err
		}
		if active, found, err := txGetActiveTrafficPeriod(ctx, tx, serverID, allowance.Scope, allowance.Direction, at); err != nil {
			return contracts.TrafficPeriod{}, err
		} else if found {
			period = active
		}
		return txAddTrafficUsage(ctx, tx, serverID, period, delta, continuity)
	}
	effective := allowance
	var transitionStart time.Time
	previous, found, err := txGetTrafficAllowance(ctx, tx, serverID, allowance.Scope, allowance.Direction)
	if err != nil {
		return contracts.TrafficPeriod{}, err
	}
	pending, hasPending, err := txGetTrafficAllowanceChange(ctx, tx, serverID, allowance.Scope, allowance.Direction)
	if err != nil {
		return contracts.TrafficPeriod{}, err
	}
	if startNewPeriod {
		if hasPending {
			if _, err := tx.ExecContext(ctx, `DELETE FROM traffic_allowance_changes WHERE server_id=? AND scope=? AND direction=?`, string(serverID), allowance.Scope, allowance.Direction); err != nil {
				return contracts.TrafficPeriod{}, err
			}
		}
		period, err := newPeriodFor(at, allowance)
		if err != nil {
			return contracts.TrafficPeriod{}, err
		}
		if persistAllowance {
			if err := txUpsertTrafficAllowance(ctx, tx, serverID, allowance, at); err != nil {
				return contracts.TrafficPeriod{}, err
			}
		}
		if err := txHandoffTrafficPeriod(ctx, tx, serverID, period); err != nil {
			return contracts.TrafficPeriod{}, err
		}
		return txAddTrafficUsage(ctx, tx, serverID, period, delta, continuity)
	}
	if hasPending {
		if !at.Before(pending.EffectiveAt) {
			transitionStart = pending.EffectiveAt.UTC()
			if _, err := tx.ExecContext(ctx, `DELETE FROM traffic_allowance_changes WHERE server_id=? AND scope=? AND direction=?`, string(serverID), allowance.Scope, allowance.Direction); err != nil {
				return contracts.TrafficPeriod{}, err
			}
		} else {
			effective = applyOldSchedule(pending.Previous, allowance)
			pending.Proposed = allowance
			if err := txSaveTrafficAllowanceChange(ctx, tx, pending); err != nil {
				return contracts.TrafficPeriod{}, err
			}
		}
	} else if found && allowanceScheduleChanged(previous, allowance) {
		oldPeriod, err := PeriodFor(at, previous)
		if err != nil {
			return contracts.TrafficPeriod{}, err
		}
		if at.Equal(oldPeriod.From) {
			effective = allowance
			transitionStart = at
		} else if at.Before(oldPeriod.To) {
			pending = monitoring.TrafficAllowanceChange{ServerID: serverID, Scope: allowance.Scope, Direction: allowance.Direction, EffectiveAt: oldPeriod.To, Previous: previous, Proposed: allowance}
			if err := txSaveTrafficAllowanceChange(ctx, tx, pending); err != nil {
				return contracts.TrafficPeriod{}, err
			}
			effective = applyOldSchedule(previous, allowance)
		}
	}
	period, err := PeriodFor(at, effective)
	if err != nil {
		return contracts.TrafficPeriod{}, err
	}
	if !transitionStart.IsZero() {
		transitionPeriod := periodStartingAt(transitionStart, allowance)
		if at.Before(transitionPeriod.To) {
			period = transitionPeriod
		}
	}
	if transitionStart.IsZero() {
		if active, found, err := txGetActiveTrafficPeriod(ctx, tx, serverID, effective.Scope, effective.Direction, at); err != nil {
			return contracts.TrafficPeriod{}, err
		} else if found {
			if persistAllowance {
				active.AllowanceBytes = effective.AllowanceBytes
				active.Timezone = effective.Timezone
				active.Interfaces = append([]string(nil), effective.Interfaces...)
				interfaces, marshalErr := json.Marshal(active.Interfaces)
				if marshalErr != nil {
					return contracts.TrafficPeriod{}, marshalErr
				}
				if _, updateErr := tx.ExecContext(ctx, `UPDATE traffic_periods SET timezone=?,allowance_bytes=?,interfaces_json=? WHERE server_id=? AND scope=? AND period_start=? AND direction=?`, active.Timezone, strconv.FormatUint(active.AllowanceBytes, 10), string(interfaces), string(serverID), active.Scope, monitoring.FormatPersistedTime(active.From), active.Direction); updateErr != nil {
					return contracts.TrafficPeriod{}, updateErr
				}
			}
			period = active
		}
	}
	if persistAllowance {
		if err := txUpsertTrafficAllowance(ctx, tx, serverID, allowance, at); err != nil {
			return contracts.TrafficPeriod{}, err
		}
	}
	return txAddTrafficUsage(ctx, tx, serverID, period, delta, continuity)
}

func txGetTrafficAllowance(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, scope, direction string) (contracts.TrafficAllowance, bool, error) {
	var allowance contracts.TrafficAllowance
	var interfacesJSON, bytesText, warningsJSON string
	var resetDay int
	err := tx.QueryRowContext(ctx, `SELECT scope,direction,interfaces_json,allowance_bytes,reset_day,timezone,warnings_json FROM traffic_allowances WHERE server_id=? AND scope=? AND direction=?`, string(serverID), scope, direction).Scan(&allowance.Scope, &allowance.Direction, &interfacesJSON, &bytesText, &resetDay, &allowance.Timezone, &warningsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.TrafficAllowance{}, false, nil
	}
	if err != nil {
		return contracts.TrafficAllowance{}, false, err
	}
	if err := json.Unmarshal([]byte(interfacesJSON), &allowance.Interfaces); err != nil {
		return contracts.TrafficAllowance{}, false, err
	}
	if err := json.Unmarshal([]byte(warningsJSON), &allowance.WarningPercentages); err != nil {
		return contracts.TrafficAllowance{}, false, err
	}
	var parseErr error
	allowance.AllowanceBytes, parseErr = strconv.ParseUint(bytesText, 10, 64)
	if parseErr != nil || resetDay < 1 || resetDay > 31 {
		return contracts.TrafficAllowance{}, false, errors.New("invalid persisted traffic allowance")
	}
	allowance.ResetDay = uint8(resetDay)
	return allowance, true, allowance.Validate()
}

func txGapCovers(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, epoch contracts.CollectorEpoch, from, to uint64) (bool, error) {
	return monitoring.CoverageGapCoversTx(ctx, tx, serverID, epoch, from, to)
}

func txGetTrafficAllowanceChange(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, scope, direction string) (monitoring.TrafficAllowanceChange, bool, error) {
	var effective, previousJSON, proposedJSON string
	err := tx.QueryRowContext(ctx, `SELECT effective_at,previous_allowance_json,proposed_allowance_json FROM traffic_allowance_changes WHERE server_id=? AND scope=? AND direction=?`, string(serverID), scope, direction).Scan(&effective, &previousJSON, &proposedJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return monitoring.TrafficAllowanceChange{}, false, nil
	}
	if err != nil {
		return monitoring.TrafficAllowanceChange{}, false, err
	}
	change := monitoring.TrafficAllowanceChange{ServerID: serverID, Scope: scope, Direction: direction}
	if change.EffectiveAt, err = time.Parse(time.RFC3339Nano, effective); err != nil {
		return monitoring.TrafficAllowanceChange{}, false, err
	}
	if err := json.Unmarshal([]byte(previousJSON), &change.Previous); err != nil {
		return monitoring.TrafficAllowanceChange{}, false, err
	}
	if err := json.Unmarshal([]byte(proposedJSON), &change.Proposed); err != nil {
		return monitoring.TrafficAllowanceChange{}, false, err
	}
	if err := change.Previous.Validate(); err != nil {
		return monitoring.TrafficAllowanceChange{}, false, err
	}
	if err := change.Proposed.Validate(); err != nil {
		return monitoring.TrafficAllowanceChange{}, false, err
	}
	return change, true, nil
}

func txSaveTrafficAllowanceChange(ctx context.Context, tx *sql.Tx, change monitoring.TrafficAllowanceChange) error {
	previous, err := json.Marshal(change.Previous)
	if err != nil {
		return err
	}
	proposed, err := json.Marshal(change.Proposed)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO traffic_allowance_changes(server_id,scope,direction,effective_at,previous_allowance_json,proposed_allowance_json) VALUES(?,?,?,?,?,?) ON CONFLICT(server_id,scope,direction) DO UPDATE SET effective_at=excluded.effective_at,previous_allowance_json=excluded.previous_allowance_json,proposed_allowance_json=excluded.proposed_allowance_json`, string(change.ServerID), change.Scope, change.Direction, monitoring.FormatPersistedTime(change.EffectiveAt), string(previous), string(proposed))
	return err
}

func txUpsertTrafficAllowance(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, allowance contracts.TrafficAllowance, effectiveAt time.Time) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM traffic_allowances WHERE server_id=?`, string(serverID)).Scan(&count); err != nil {
		return err
	}
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM traffic_allowances WHERE server_id=? AND scope=? AND direction=?)`, string(serverID), allowance.Scope, allowance.Direction).Scan(&existing); err != nil {
		return err
	}
	if existing == 0 && count >= monitoring.MaxTrafficAllowances {
		return monitoring.ErrTrafficAllowanceLimit
	}
	interfaces, err := json.Marshal(allowance.Interfaces)
	if err != nil {
		return err
	}
	warnings, err := json.Marshal(allowance.WarningPercentages)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO traffic_allowances(server_id,scope,direction,interfaces_json,allowance_bytes,reset_day,timezone,warnings_json) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(server_id,scope,direction) DO UPDATE SET interfaces_json=excluded.interfaces_json,allowance_bytes=excluded.allowance_bytes,reset_day=excluded.reset_day,timezone=excluded.timezone,warnings_json=excluded.warnings_json`, string(serverID), allowance.Scope, allowance.Direction, string(interfaces), strconv.FormatUint(allowance.AllowanceBytes, 10), allowance.ResetDay, allowance.Timezone, string(warnings)); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO traffic_allowance_versions(server_id,scope,direction,effective_at,interfaces_json,allowance_bytes,reset_day,timezone,warnings_json) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(server_id,scope,direction,effective_at) DO UPDATE SET interfaces_json=excluded.interfaces_json,allowance_bytes=excluded.allowance_bytes,reset_day=excluded.reset_day,timezone=excluded.timezone,warnings_json=excluded.warnings_json`, string(serverID), allowance.Scope, allowance.Direction, monitoring.FormatPersistedTime(effectiveAt), string(interfaces), strconv.FormatUint(allowance.AllowanceBytes, 10), allowance.ResetDay, allowance.Timezone, string(warnings))
	return err
}

func txGetTrafficAllowanceAt(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, scope, direction string, acceptedAt time.Time) (contracts.TrafficAllowance, bool, error) {
	var allowance contracts.TrafficAllowance
	var interfacesJSON, bytesText, warningsJSON string
	var resetDay int
	err := tx.QueryRowContext(ctx, `SELECT scope,direction,interfaces_json,allowance_bytes,reset_day,timezone,warnings_json FROM traffic_allowance_versions WHERE server_id=? AND scope=? AND direction=? AND effective_at<=? ORDER BY effective_at DESC LIMIT 1`, string(serverID), scope, direction, monitoring.FormatPersistedTime(acceptedAt)).Scan(&allowance.Scope, &allowance.Direction, &interfacesJSON, &bytesText, &resetDay, &allowance.Timezone, &warningsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.TrafficAllowance{}, false, nil
	}
	if err != nil {
		return contracts.TrafficAllowance{}, false, err
	}
	if err := json.Unmarshal([]byte(interfacesJSON), &allowance.Interfaces); err != nil {
		return contracts.TrafficAllowance{}, false, err
	}
	if err := json.Unmarshal([]byte(warningsJSON), &allowance.WarningPercentages); err != nil {
		return contracts.TrafficAllowance{}, false, err
	}
	allowance.AllowanceBytes, err = strconv.ParseUint(bytesText, 10, 64)
	if err != nil || resetDay < 1 || resetDay > 31 {
		return contracts.TrafficAllowance{}, false, errors.New("invalid traffic allowance version")
	}
	allowance.ResetDay = uint8(resetDay)
	if err := allowance.Validate(); err != nil {
		return contracts.TrafficAllowance{}, false, err
	}
	return allowance, true, nil
}

func txListTrafficPeriodsIntersecting(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, scope, direction string, start, end time.Time) ([]contracts.TrafficPeriod, error) {
	if !end.After(start) {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT scope,period_start,period_end,timezone,allowance_bytes,direction,counted_bytes,continuity,interfaces_json FROM traffic_periods WHERE server_id=? AND scope=? AND direction=? AND period_end > ? AND period_start < ? ORDER BY period_start ASC, period_end ASC LIMIT 65`, string(serverID), scope, direction, monitoring.FormatPersistedTime(start), monitoring.FormatPersistedTime(end))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	periods := make([]contracts.TrafficPeriod, 0, 4)
	for rows.Next() {
		var period contracts.TrafficPeriod
		var from, to, allowance, counted, interfaces string
		if err := rows.Scan(&period.Scope, &from, &to, &period.Timezone, &allowance, &period.Direction, &counted, &period.Continuity, &interfaces); err != nil {
			return nil, err
		}
		var parseErr error
		period.From, parseErr = time.Parse(time.RFC3339Nano, from)
		if parseErr != nil {
			return nil, parseErr
		}
		period.To, parseErr = time.Parse(time.RFC3339Nano, to)
		if parseErr != nil {
			return nil, parseErr
		}
		period.AllowanceBytes, parseErr = strconv.ParseUint(allowance, 10, 64)
		if parseErr != nil {
			return nil, parseErr
		}
		period.CountedBytes, parseErr = strconv.ParseUint(counted, 10, 64)
		if parseErr != nil {
			return nil, parseErr
		}
		if err := json.Unmarshal([]byte(interfaces), &period.Interfaces); err != nil {
			return nil, err
		}
		if err := period.Validate(); err != nil {
			return nil, err
		}
		periods = append(periods, period)
		if len(periods) > 64 {
			return periods[:64], errTrafficSplitBounded
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return periods, nil
}

func txGetActiveTrafficPeriod(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, scope, direction string, at time.Time) (contracts.TrafficPeriod, bool, error) {
	var period contracts.TrafficPeriod
	var from, to, allowance, counted, interfaces string
	err := tx.QueryRowContext(ctx, `SELECT scope,period_start,period_end,timezone,allowance_bytes,direction,counted_bytes,continuity,interfaces_json FROM traffic_periods WHERE server_id=? AND scope=? AND direction=? AND period_start <= ? AND period_end > ? ORDER BY period_start DESC LIMIT 1`, string(serverID), scope, direction, monitoring.FormatPersistedTime(at), monitoring.FormatPersistedTime(at)).Scan(&period.Scope, &from, &to, &period.Timezone, &allowance, &period.Direction, &counted, &period.Continuity, &interfaces)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.TrafficPeriod{}, false, nil
	}
	if err != nil {
		return contracts.TrafficPeriod{}, false, err
	}
	if period.From, err = time.Parse(time.RFC3339Nano, from); err != nil {
		return contracts.TrafficPeriod{}, false, err
	}
	if period.To, err = time.Parse(time.RFC3339Nano, to); err != nil {
		return contracts.TrafficPeriod{}, false, err
	}
	if period.AllowanceBytes, err = strconv.ParseUint(allowance, 10, 64); err != nil {
		return contracts.TrafficPeriod{}, false, err
	}
	if period.CountedBytes, err = strconv.ParseUint(counted, 10, 64); err != nil {
		return contracts.TrafficPeriod{}, false, err
	}
	if err := json.Unmarshal([]byte(interfaces), &period.Interfaces); err != nil {
		return contracts.TrafficPeriod{}, false, err
	}
	return period, true, period.Validate()
}

func txNextTrafficPeriodStart(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, scope, direction string, after, before time.Time) (time.Time, bool, error) {
	var start string
	err := tx.QueryRowContext(ctx, `SELECT period_start FROM traffic_periods WHERE server_id=? AND scope=? AND direction=? AND period_start > ? AND period_start < ? ORDER BY period_start ASC LIMIT 1`, string(serverID), scope, direction, monitoring.FormatPersistedTime(after), monitoring.FormatPersistedTime(before)).Scan(&start)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	value, err := time.Parse(time.RFC3339Nano, start)
	if err != nil {
		return time.Time{}, false, err
	}
	return value, true, nil
}

func txAddTrafficUsage(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, period contracts.TrafficPeriod, delta uint64, continuity string) (contracts.TrafficPeriod, error) {
	if ^uint64(0)-period.CountedBytes < delta {
		return contracts.TrafficPeriod{}, errors.New("traffic usage exceeds uint64")
	}
	from, to := monitoring.FormatPersistedTime(period.From), monitoring.FormatPersistedTime(period.To)
	interfaces, err := json.Marshal(period.Interfaces)
	if err != nil {
		return contracts.TrafficPeriod{}, err
	}
	overlaps, err := txTrafficPeriodOverlaps(ctx, tx, serverID, period)
	if err != nil {
		return contracts.TrafficPeriod{}, err
	}
	if overlaps {
		return contracts.TrafficPeriod{}, errors.New("traffic period overlaps an existing period")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO traffic_periods(server_id,scope,period_start,period_end,timezone,allowance_bytes,direction,counted_bytes,continuity,interfaces_json) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(server_id,scope,period_start,direction) DO NOTHING`, string(serverID), period.Scope, from, to, period.Timezone, strconv.FormatUint(period.AllowanceBytes, 10), period.Direction, "0", period.Continuity, string(interfaces)); err != nil {
		return contracts.TrafficPeriod{}, err
	}
	var existingTo, existingTimezone, allowanceText, countedText, existingContinuity, existingInterfaces string
	if err := tx.QueryRowContext(ctx, `SELECT period_end,timezone,allowance_bytes,counted_bytes,continuity,interfaces_json FROM traffic_periods WHERE server_id=? AND scope=? AND period_start=? AND direction=?`, string(serverID), period.Scope, from, period.Direction).Scan(&existingTo, &existingTimezone, &allowanceText, &countedText, &existingContinuity, &existingInterfaces); err != nil {
		return contracts.TrafficPeriod{}, err
	}
	// Preserve every identity/configuration field already attached to this
	// durable period. A delayed sample is usage evidence, not authorization to
	// relabel historical allowance policy with today's configuration.
	period.To, err = time.Parse(time.RFC3339Nano, existingTo)
	if err != nil {
		return contracts.TrafficPeriod{}, err
	}
	period.Timezone = existingTimezone
	period.AllowanceBytes, err = strconv.ParseUint(allowanceText, 10, 64)
	if err != nil {
		return contracts.TrafficPeriod{}, errors.New("invalid traffic allowance bytes")
	}
	if err := json.Unmarshal([]byte(existingInterfaces), &period.Interfaces); err != nil {
		return contracts.TrafficPeriod{}, err
	}
	counted, err := strconv.ParseUint(countedText, 10, 64)
	if err != nil || ^uint64(0)-counted < delta {
		return contracts.TrafficPeriod{}, errors.New("traffic usage exceeds uint64")
	}
	counted += delta
	merged := mergeContinuity(existingContinuity, continuity)
	if _, err := tx.ExecContext(ctx, `UPDATE traffic_periods SET counted_bytes=?,continuity=? WHERE server_id=? AND scope=? AND period_start=? AND direction=?`, strconv.FormatUint(counted, 10), merged, string(serverID), period.Scope, from, period.Direction); err != nil {
		return contracts.TrafficPeriod{}, err
	}
	period.CountedBytes, period.Continuity = counted, merged
	return period, nil
}

// txHandoffTrafficPeriod atomically closes the period(s) active immediately
// before an explicit owner-requested restart. A future period inside the new
// interval is rejected rather than silently creating two active identities.
func txHandoffTrafficPeriod(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, period contracts.TrafficPeriod) error {
	from := monitoring.FormatPersistedTime(period.From)
	if _, err := tx.ExecContext(ctx, `UPDATE traffic_periods SET period_end=? WHERE server_id=? AND scope=? AND direction=? AND period_start < ? AND period_end > ?`, from, string(serverID), period.Scope, period.Direction, from, from); err != nil {
		return err
	}
	overlaps, err := txTrafficPeriodOverlaps(ctx, tx, serverID, period)
	if err != nil {
		return err
	}
	if overlaps {
		return errors.New("explicit traffic period overlaps an existing future period")
	}
	return nil
}

func txTrafficPeriodOverlaps(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, period contracts.TrafficPeriod) (bool, error) {
	var exists int
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM traffic_periods WHERE server_id=? AND scope=? AND direction=? AND period_start<>? AND period_start < ? AND period_end > ?)`, string(serverID), period.Scope, period.Direction, monitoring.FormatPersistedTime(period.From), monitoring.FormatPersistedTime(period.To), monitoring.FormatPersistedTime(period.From)).Scan(&exists)
	return exists == 1, err
}

func mergeContinuity(left, right string) string {
	if left == "gap" || right == "gap" {
		return "gap"
	}
	if left == "uncertain" || right == "uncertain" {
		return "uncertain"
	}
	return "complete"
}

// CounterDelta translates collector counter samples into one billing delta.
// It returns continuity="uncertain" on a reset, epoch change, unavailable
// selected interface, out-of-order sequence, or non-monotonic counter; callers
// must retain that uncertainty instead of turning it into zero traffic. An
// empty interface selection uses the collector's explicitly filtered
// net.billing counters.
func CounterDelta(previous, current contracts.MetricSample, allowance contracts.TrafficAllowance) (uint64, string, error) {
	allowance = WithDefaults(allowance)
	if err := allowance.Validate(); err != nil {
		return 0, "uncertain", err
	}
	if previous.ServerID != current.ServerID || previous.ServerID == "" || previous.CollectorEpoch != current.CollectorEpoch || !current.ObservedAt.After(previous.ObservedAt) || current.Sequence <= previous.Sequence || previous.Sequence == ^uint64(0) || current.Sequence != previous.Sequence+1 || timestampUncertain(previous.TimestampUncertainty) || timestampUncertain(current.TimestampUncertainty) {
		return 0, "uncertain", nil
	}
	keys := allowance.Interfaces
	if len(keys) == 0 {
		keys = []string{"billing"}
	}
	var previousTotal, currentTotal uint64
	for _, iface := range keys {
		prefix := "net.billing."
		if iface != "billing" {
			prefix = "net." + sanitizeInterface(iface) + "."
		}
		for _, direction := range []string{"rx_bytes", "tx_bytes"} {
			if allowance.Direction == "inbound" && direction != "rx_bytes" || allowance.Direction == "outbound" && direction != "tx_bytes" {
				continue
			}
			previousText, previousOK := previous.Counters[prefix+direction]
			currentText, currentOK := current.Counters[prefix+direction]
			if !previousOK || !currentOK || previous.Validity[prefix+direction] != "valid" || current.Validity[prefix+direction] != "valid" {
				return 0, "uncertain", nil
			}
			before, beforeErr := parseCanonicalUint64(previousText)
			after, afterErr := parseCanonicalUint64(currentText)
			if beforeErr != nil || afterErr != nil {
				return 0, "uncertain", nil
			}
			if after < before {
				return 0, "uncertain", nil
			}
			var ok bool
			previousTotal, ok = addUint64(previousTotal, before)
			if !ok {
				return 0, "uncertain", nil
			}
			currentTotal, ok = addUint64(currentTotal, after)
			if !ok {
				return 0, "uncertain", nil
			}
		}
	}
	if currentTotal < previousTotal {
		return 0, "uncertain", nil
	}
	return currentTotal - previousTotal, "complete", nil
}

func parseCanonicalUint64(value string) (uint64, error) {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return 0, errors.New("counter is not a canonical decimal string")
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, errors.New("counter is not a decimal string")
		}
	}
	return strconv.ParseUint(value, 10, 64)
}

func timestampUncertain(value string) bool {
	return value != "" && value != "none"
}

func sanitizeInterface(value string) string {
	var builder strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			builder.WriteRune(r)
		} else {
			builder.WriteByte('_')
		}
	}
	return builder.String()
}

func addUint64(left, right uint64) (uint64, bool) {
	if ^uint64(0)-left < right {
		return 0, false
	}
	return left + right, true
}

// Forecast computes a deliberately labelled recent-rate estimate. At least 24
// hours of valid observations and 80% usable coverage are required; missing or
// reset counter intervals never become zero traffic.
func Forecast(period contracts.TrafficPeriod, observations []contracts.TrafficObservation, asOf time.Time) (contracts.TrafficForecast, error) {
	return forecastWithObservedBytes(period, observations, asOf, nil)
}

// forecastWithObservedBytes is the internal form used by the HTTP adapter.
// A non-nil observedBytes is a bounded count at asOf obtained by replaying
// durable counter samples. Keeping this override internal preserves the public
// Forecast API while preventing an API request from accidentally treating a
// period's eventual CountedBytes as an as-of value.
func forecastWithObservedBytes(period contracts.TrafficPeriod, observations []contracts.TrafficObservation, asOf time.Time, observedBytes *uint64) (contracts.TrafficForecast, error) {
	return forecastWithObservedBytesWindow(period, observations, asOf, observedBytes, nil)
}

func forecastWithObservedBytesWindow(period contracts.TrafficPeriod, observations []contracts.TrafficObservation, asOf time.Time, observedBytes *uint64, recentStart *time.Time) (contracts.TrafficForecast, error) {
	if err := period.Validate(); err != nil {
		return contracts.TrafficForecast{}, err
	}
	if asOf.IsZero() {
		return contracts.TrafficForecast{}, errors.New("forecast timestamp is required")
	}
	asOf = asOf.UTC()
	result := contracts.TrafficForecast{AsOf: asOf, PeriodEnd: period.To.UTC()}
	// CountedBytes is not an authoritative baseline once any reset, gap, or
	// uncertain interval has affected the target period. Clean observations
	// later in the same period can establish a recent rate, but they cannot
	// reconstruct the bytes omitted before continuity was lost.
	if period.Continuity != "complete" {
		result.Reason = "traffic_period_continuity_incomplete"
		return result, nil
	}
	observedBytesKnown := observedBytes != nil
	if observedBytesKnown {
		result.ObservedBytes = *observedBytes
	} else {
		// Preserve the public library contract for callers that only have a
		// durable period total. The HTTP adapter supplies an explicit override
		// (and uses zero when a bounded as-of total cannot be established).
		result.ObservedBytes = period.CountedBytes
	}
	ordered := append([]contracts.TrafficObservation(nil), observations...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ObservedAt.Before(ordered[j].ObservedAt) })
	validCount := 0
	for _, observation := range ordered {
		if err := observation.Validate(); err != nil {
			continue
		}
		when := observation.ObservedAt.UTC()
		if when.Before(period.From.UTC()) || !when.Before(period.To.UTC()) || when.After(asOf) {
			continue
		}
		validCount++
	}
	if validCount < 2 {
		result.Reason = "at_least_24_hours_of_usable_data_required"
		return result, nil
	}
	// "Recent" is relative to the caller's as-of instant, not to the last
	// sample received. Otherwise a stale stream whose last point is several
	// weeks old could still produce a seemingly fresh forecast by moving the
	// window backwards with that point.
	windowEnd := asOf
	if period.To.UTC().Before(windowEnd) {
		windowEnd = period.To.UTC()
	}
	windowStart := windowEnd.Add(-RecentForecast)
	if recentStart != nil && recentStart.UTC().After(windowStart) {
		windowStart = recentStart.UTC()
	}
	coverageStart := period.From.UTC()
	if coverageStart.Before(windowStart) {
		coverageStart = windowStart
	}
	coverageSpan := windowEnd.Sub(coverageStart)
	if coverageSpan <= 0 {
		result.Reason = "at_least_24_hours_of_usable_data_required"
		return result, nil
	}
	var usable time.Duration
	var rateDuration time.Duration
	var bytes uint64
	var haveCounter bool
	var previous contracts.TrafficObservation
	for _, observation := range ordered {
		when := observation.ObservedAt.UTC()
		if when.Before(period.From.UTC()) || !when.Before(period.To.UTC()) || when.After(asOf) {
			continue
		}
		if err := observation.Validate(); err != nil {
			// An explicitly invalid point is a coverage break. Do not bridge
			// across it and treat the two surrounding counters as one usable
			// interval.
			haveCounter = false
			continue
		}
		if when.Before(windowStart) {
			previous, haveCounter = observation, true
			continue
		}
		if haveCounter {
			interval := when.Sub(previous.ObservedAt.UTC())
			if interval > 0 && interval <= MaxForecastInterval {
				coverage := previous.Coverage
				if observation.Coverage < coverage {
					coverage = observation.Coverage
				}
				if coverage >= 0.8 {
					before, beforeOK := counterValue(previous, period.Direction)
					after, afterOK := counterValue(observation, period.Direction)
					// Counter resets/overflow are not zero-traffic intervals. Leave
					// their duration out of the usable window so the estimate is
					// withheld rather than quietly biased low.
					if beforeOK && afterOK && after >= before && ^uint64(0)-bytes >= after-before {
						// Use the complete counter interval for its measured rate, but
						// count only its overlap with the requested recent window as
						// usable coverage. This admits ordinary cadence jitter around
						// the exact 24-hour boundary without inventing bytes.
						overlapStart := previous.ObservedAt.UTC()
						if overlapStart.Before(windowStart) {
							overlapStart = windowStart
						}
						if when.After(overlapStart) {
							usable += when.Sub(overlapStart)
							rateDuration += interval
						}
						bytes += after - before
					}
				}
			}
		}
		previous, haveCounter = observation, true
	}
	result.UsableDurationSeconds = int64(usable / time.Second)
	result.Coverage = float64(usable) / float64(coverageSpan)
	if result.Coverage > 1 {
		result.Coverage = 1
	}
	if usable < MinimumForecast || result.Coverage < 0.8 {
		result.Reason = "coverage_is_too_sparse_for_a_reliable_forecast"
		return result, nil
	}
	if !observedBytesKnown && result.ObservedBytes == 0 {
		result.ObservedBytes = bytes
	}
	if rateDuration <= 0 {
		result.Reason = "no_monotonic_counter_rate_available"
		return result, nil
	}
	rate := float64(bytes) / rateDuration.Seconds()
	remaining := period.To.UTC().Sub(asOf.UTC())
	if remaining < 0 {
		remaining = 0
	}
	estimate := float64(result.ObservedBytes) + rate*remaining.Seconds()
	if estimate > float64(^uint64(0)) || math.IsInf(estimate, 0) {
		result.Reason = "forecast_exceeds_uint64"
		return result, nil
	}
	result.EstimatedBytesAtEnd = uint64(estimate)
	if result.EstimatedBytesAtEnd >= result.ObservedBytes {
		result.EstimatedRemaining = result.EstimatedBytesAtEnd - result.ObservedBytes
	}
	result.Available = true
	result.Label = "estimated_recent_rate"
	return result, nil
}

// observedBytesAtAsOf derives a bounded period total from durable samples. The
// effective observation is the latest valid sample at or before asOf; when a
// collector does not report exactly at the requested instant, the returned
// value is therefore a conservative lower bound rather than a future period
// total. Invalid samples between the first and effective observations prevent
// even that bounded replay from being trusted.
func observedBytesAtAsOf(period contracts.TrafficPeriod, observations []contracts.TrafficObservation, asOf time.Time) (uint64, bool) {
	from := period.From.UTC()
	end := asOf.UTC()
	if period.To.UTC().Before(end) {
		end = period.To.UTC()
	}
	if end.Before(from) {
		return 0, false
	}
	ordered := append([]contracts.TrafficObservation(nil), observations...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ObservedAt.Before(ordered[j].ObservedAt) })
	startIndex, endIndex := -1, -1
	for index, observation := range ordered {
		when := observation.ObservedAt.UTC()
		if when.Before(from) || when.After(end) {
			continue
		}
		if observation.Validate() == nil && observation.Coverage >= 0.8 {
			if startIndex == -1 {
				startIndex = index
			}
			endIndex = index
		}
	}
	if startIndex == -1 || endIndex == -1 {
		return 0, false
	}
	// A period total cannot be reconstructed from an arbitrary recent sample:
	// doing so would silently omit all traffic before the forecast window. Only
	// an exact period-boundary baseline is authoritative for an explicit
	// historical as_of request.
	if !ordered[startIndex].ObservedAt.UTC().Equal(from) {
		return 0, false
	}
	baseline, ok := counterValue(ordered[startIndex], period.Direction)
	if !ok {
		return 0, false
	}
	previous := baseline
	previousAt := ordered[startIndex].ObservedAt.UTC()
	for _, observation := range ordered[startIndex : endIndex+1] {
		if err := observation.Validate(); err != nil || observation.Coverage < 0.8 {
			return 0, false
		}
		when := observation.ObservedAt.UTC()
		if !when.Equal(previousAt) && (when.Before(previousAt) || when.Sub(previousAt) > MaxForecastInterval) {
			// Missing samples are not zero traffic. Without a bounded interval
			// we cannot claim an exact period total at the requested boundary.
			return 0, false
		}
		value, valueOK := counterValue(observation, period.Direction)
		if !valueOK || value < previous {
			return 0, false
		}
		previous = value
		previousAt = when
	}
	return previous - baseline, true
}

func counterValue(observation contracts.TrafficObservation, direction string) (uint64, bool) {
	if direction == "inbound" {
		return observation.InboundBytes, true
	}
	if direction == "outbound" {
		return observation.OutboundBytes, true
	}
	if ^uint64(0)-observation.InboundBytes < observation.OutboundBytes {
		return 0, false
	}
	return observation.InboundBytes + observation.OutboundBytes, true
}

func (p PeriodPreview) String() string {
	return fmt.Sprintf("%s -> %s (%s)", p.Current.From.Format(time.RFC3339), p.Proposed.To.Format(time.RFC3339), p.ScheduleEffect)
}
