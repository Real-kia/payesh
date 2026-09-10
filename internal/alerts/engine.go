// Package alerts evaluates timestamped metric observations into durable alert
// state. It intentionally produces warnings only; it never invokes a control
// policy or changes a node's enforcement configuration.
package alerts

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

const (
	DefaultAlertDuration    = 5 * time.Minute
	DefaultReminder         = time.Hour
	MinimumCoverage         = 0.8
	MaxNotificationBody     = 64 << 10
	maxDeliveryReservations = maxPendingNotifications + 8
)

type Observation struct {
	ServerID    contracts.ServerID
	ObservedAt  time.Time
	Values      map[string]float64
	Validity    map[string]string
	Coverage    float64
	Unreachable bool
}

type Evaluation struct {
	State        contracts.AlertState
	Changed      bool
	Notify       bool
	Suppressed   bool
	Notification string
	Reason       string
}

type parsedExpression struct {
	metric    string
	operator  string
	threshold float64
}

var expressionPattern = regexp.MustCompile(`^([A-Za-z0-9_.:-]{1,128})\s*(>=|<=|==|!=|>|<)\s*(-?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+))$`)

type Engine struct {
	Store         *monitoring.Store
	Now           func() time.Time
	DeliveryQueue *DeliveryQueue
	Destinations  func(context.Context, contracts.ServerID) []NotificationDestination
	mu            sync.Mutex
	outageMu      sync.RWMutex
	outages       map[contracts.ServerID]time.Time
	deliveryMu    sync.Mutex
	deliveries    map[string]struct{}
}

func NewEngine(store *monitoring.Store) (*Engine, error) {
	if store == nil {
		return nil, errors.New("monitoring store is required")
	}
	return &Engine{Store: store, Now: func() time.Time { return time.Now().UTC() }, outages: make(map[contracts.ServerID]time.Time), deliveries: make(map[string]struct{})}, nil
}

// ConfigureDelivery connects committed alert events to the bounded delivery
// queue. Destination secrets stay with the provider and are never persisted
// as part of alert state or history.
func (e *Engine) ConfigureDelivery(queue *DeliveryQueue, destinations func(context.Context, contracts.ServerID) []NotificationDestination) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.DeliveryQueue = queue
	e.Destinations = destinations
	e.mu.Unlock()
}

func (e *Engine) now() time.Time {
	if e.Now == nil {
		return time.Now().UTC()
	}
	return e.Now().UTC()
}

func ParseExpression(expression string) (string, string, float64, error) {
	matches := expressionPattern.FindStringSubmatch(strings.TrimSpace(expression))
	if len(matches) != 4 {
		return "", "", 0, errors.New("expression must be metric operator number")
	}
	threshold, err := strconv.ParseFloat(matches[3], 64)
	if err != nil || math.IsNaN(threshold) || math.IsInf(threshold, 0) {
		return "", "", 0, errors.New("expression threshold is invalid")
	}
	return matches[1], matches[2], threshold, nil
}

func (e *Engine) AddRule(ctx context.Context, rule contracts.AlertRule) error {
	if _, _, _, err := ParseExpression(rule.Expression); err != nil {
		return err
	}
	if rule.DurationSeconds == 0 {
		rule.DurationSeconds = int64(DefaultAlertDuration / time.Second)
	}
	// A zero reminder interval is a deliberate, contract-supported way to
	// disable sustained reminders. HTTP creation applies the default when the
	// field is omitted; the engine must preserve an explicit zero for callers
	// that construct a rule directly.
	if rule.CreatedAt.IsZero() {
		rule.CreatedAt = e.now()
	}
	if rule.EffectiveAt.IsZero() {
		rule.EffectiveAt = rule.CreatedAt
	}
	return e.Store.CreateAlertRule(ctx, rule)
}

func (e *Engine) EnsureStarterRules(ctx context.Context, serverID contracts.ServerID) error {
	for _, rule := range StarterRules(serverID) {
		// Starter rules are defaults, not a reconciliation policy. Once an
		// owner edits a rule, a server restart must not silently restore the
		// original threshold or duration.
		if _, found, err := e.Store.GetAlertRule(ctx, rule.ID); err != nil {
			return err
		} else if found {
			continue
		}
		if err := e.AddRule(ctx, rule); err != nil {
			return err
		}
	}
	// Older releases installed a generic traffic starter that neither the raw
	// sample path nor the allowance-scoped path could evaluate. Disable every
	// such legacy record (including owner edits retaining its starter identity)
	// and terminalize any state through the normal transactional disable path.
	return e.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		rules, err := e.Store.ListAlertRulesByServerTx(ctx, tx, serverID, monitoring.MaxAlertRulesPerServer)
		if err != nil {
			return err
		}
		for _, rule := range rules {
			if !isLegacyGenericTrafficStarter(rule) || !rule.Enabled {
				continue
			}
			rule.Enabled = false
			rule.DisableReason = "superseded_by_allowance_scoped_traffic_rules"
			if err := e.Store.SaveAlertRuleTx(ctx, tx, rule); err != nil {
				return err
			}
		}
		return nil
	})
}

func isLegacyGenericTrafficStarter(rule contracts.AlertRule) bool {
	return strings.HasPrefix(rule.ID, "traffic-high-") && strings.HasPrefix(rule.IdempotencyKey, "starter-")
}

// EnsureTrafficRules installs missing allowance warning rules without
// overwriting owner edits. Warning thresholds are configuration data, so the
// traffic service calls this whenever an allowance is first configured or
// changed.
func (e *Engine) EnsureTrafficRules(ctx context.Context, serverID contracts.ServerID, allowance contracts.TrafficAllowance) error {
	if e == nil || e.Store == nil {
		return errors.New("alert engine is unavailable")
	}
	return e.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		// Ingestion may have listed an allowance before a configuration CAS
		// committed. Read the authoritative row while holding this transaction
		// so stale warning thresholds cannot be reconciled over the new config.
		if current, found, err := trafficAllowanceTx(ctx, tx, serverID, allowance.Scope, allowance.Direction); err != nil {
			return err
		} else if found {
			allowance = current
		}
		return e.EnsureTrafficRulesTx(ctx, tx, serverID, allowance)
	})
}

func trafficAllowanceTx(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, scope, direction string) (contracts.TrafficAllowance, bool, error) {
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
	if parseErr != nil {
		return contracts.TrafficAllowance{}, false, parseErr
	}
	allowance.ResetDay = uint8(resetDay)
	return allowance, true, allowance.Validate()
}

// EnsureTrafficRulesTx reconciles generated traffic defaults through the
// caller's transaction. This is used by the traffic configuration endpoint so
// warning rules, allowance state, configuration revision, and idempotency
// record either all commit or all roll back together.
func (e *Engine) EnsureTrafficRulesTx(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID, allowance contracts.TrafficAllowance) error {
	if e == nil || e.Store == nil || tx == nil {
		return errors.New("alert engine transaction is unavailable")
	}
	allowance = trafficDefaults(allowance)
	if err := allowance.Validate(); err != nil {
		return err
	}
	desired := TrafficAlertRules(serverID, allowance)
	desiredGroup := trafficRuleGroup(allowance.Scope, allowance.Direction)
	desiredIDs := make(map[string]struct{}, len(desired))
	for _, rule := range desired {
		desiredIDs[rule.ID] = struct{}{}
		existing, found, err := e.Store.GetAlertRuleTx(ctx, tx, rule.ID)
		if err != nil {
			return err
		}
		if found {
			// Reconcile only defaults that are still untouched. In particular,
			// restore a disabled generated rule, but never resurrect an owner's
			// deliberate disable/edit.
			migrateLegacyGroup := existing.GroupKey == "traffic" && existing.GroupKey != desiredGroup
			restoreRemoved := existing.DisableReason == "threshold_removed" && !existing.Enabled
			if isUneditedGeneratedTrafficRuleFor(existing, desiredGroup) && (migrateLegacyGroup || restoreRemoved) {
				existing.GroupKey = desiredGroup
				existing.Enabled = true
				existing.DisableReason = ""
				if err := e.Store.SaveAlertRuleTx(ctx, tx, existing); err != nil {
					return err
				}
			}
			continue
		}
		if _, _, _, err := ParseExpression(rule.Expression); err != nil {
			return err
		}
		if err := e.Store.SaveAlertRuleTx(ctx, tx, rule); err != nil {
			return err
		}
	}
	// Disable obsolete generated defaults when the owner removes a warning
	// threshold. An owner-edited rule is left untouched, even if it originated
	// from an earlier allowance, because the alert rules are explicitly
	// editable policy records.
	rules, err := e.Store.ListAlertRulesByServerTx(ctx, tx, serverID, monitoring.MaxAlertRulesPerServer)
	if err != nil {
		return err
	}
	for _, rule := range rules {
		if rule.GroupKey != desiredGroup || !strings.HasPrefix(rule.IdempotencyKey, "traffic-starter-") {
			continue
		}
		if _, keep := desiredIDs[rule.ID]; keep || !rule.Enabled || !isUneditedGeneratedTrafficRule(rule) {
			continue
		}
		rule.Enabled = false
		rule.DisableReason = "threshold_removed"
		if err := e.Store.SaveAlertRuleTx(ctx, tx, rule); err != nil {
			return err
		}
	}
	return nil
}

// isUneditedGeneratedTrafficRule distinguishes a generated default from an
// owner-edited rule. Threshold removal may disable only the former; changing a
// duration, reminder, expression, or name is an explicit owner decision that
// must survive later allowance reconciliation.
func isUneditedGeneratedTrafficRule(rule contracts.AlertRule) bool {
	const prefix = "traffic-starter-"
	if !strings.HasPrefix(rule.IdempotencyKey, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(rule.IdempotencyKey, prefix)
	if suffix == "" || rule.ID != "traffic-"+suffix || (rule.GroupKey != "traffic" && !strings.HasPrefix(rule.GroupKey, "traffic-")) || rule.RecoveryThreshold != nil || rule.DurationSeconds != int64(DefaultAlertDuration/time.Second) || rule.ReminderSeconds != int64(DefaultReminder/time.Second) {
		return false
	}
	var threshold int
	if _, err := fmt.Sscanf(rule.Name, "Traffic at %d%% allowance", &threshold); err != nil || threshold < 1 || threshold > 100 {
		return false
	}
	return rule.Name == fmt.Sprintf("Traffic at %d%% allowance", threshold) && rule.Expression == "traffic.usage_percent >= "+strconv.Itoa(threshold)
}

func isUneditedGeneratedTrafficRuleFor(rule contracts.AlertRule, expectedGroup string) bool {
	if rule.GroupKey != expectedGroup && rule.GroupKey != "traffic" {
		return false
	}
	return isUneditedGeneratedTrafficRule(rule)
}

// EvaluateSample connects durable monitoring samples to the alert state
// machine. Coverage is supplied by the caller because a raw page may have
// sequence gaps that are not visible in an individual sample.
func (e *Engine) EvaluateSample(ctx context.Context, sample contracts.MetricSample, coverage float64) ([]Evaluation, error) {
	if err := sample.Validate(); err != nil {
		return nil, err
	}
	rules, err := e.listRules(ctx)
	if err != nil {
		return nil, err
	}
	results := make([]Evaluation, 0, len(rules))
	for _, rule := range rules {
		if rule.ServerID != "" && rule.ServerID != sample.ServerID {
			continue
		}
		// Traffic rules are driven by allowance-period observations, which are
		// materialized separately by EvaluateTraffic. A raw metric sample does
		// not contain traffic.usage_percent; evaluating these rules here would
		// therefore produce an uncertain observation and reset a pending clock
		// immediately before the real traffic evaluation runs.
		if metric, _, _, parseErr := ParseExpression(rule.Expression); parseErr == nil && (metric == "traffic" || strings.HasPrefix(metric, "traffic.")) {
			continue
		}
		result, evalErr := e.Evaluate(ctx, rule, Observation{ServerID: sample.ServerID, ObservedAt: sample.ObservedAt, Values: sample.Values, Validity: sample.Validity, Coverage: coverage}, sample.ObservedAt)
		if evalErr != nil {
			return nil, evalErr
		}
		results = append(results, result)
	}
	return results, nil
}

func (e *Engine) EvaluateUnreachable(ctx context.Context, serverID contracts.ServerID, at time.Time, unreachable bool) ([]Evaluation, error) {
	if serverID == "" || at.IsZero() {
		return nil, errors.New("server and observation time are required")
	}
	if unreachable {
		e.outageMu.Lock()
		e.outages[serverID] = at.UTC()
		e.outageMu.Unlock()
	} else {
		e.outageMu.Lock()
		delete(e.outages, serverID)
		e.outageMu.Unlock()
	}
	rules, err := e.listRules(ctx)
	if err != nil {
		return nil, err
	}
	results := make([]Evaluation, 0, len(rules))
	for _, rule := range rules {
		if rule.ServerID != "" && rule.ServerID != serverID {
			continue
		}
		if !strings.Contains(rule.Expression, "node.unreachable") {
			continue
		}
		result, evalErr := e.Evaluate(ctx, rule, Observation{ServerID: serverID, ObservedAt: at, Coverage: 1, Unreachable: unreachable}, at)
		if evalErr != nil {
			return nil, evalErr
		}
		results = append(results, result)
	}
	return results, nil
}

// EvaluateTraffic turns a host-side allowance total into the same alert state
// machine as resource metrics. A gap/uncertain traffic period is retained but
// receives reduced coverage, so a threshold notification is withheld until
// the accounting stream is trustworthy again.
func (e *Engine) EvaluateTraffic(ctx context.Context, serverID contracts.ServerID, period contracts.TrafficPeriod, at time.Time) ([]Evaluation, error) {
	if err := period.Validate(); err != nil {
		return nil, err
	}
	if serverID == "" || at.IsZero() {
		return nil, errors.New("traffic server and observation time are required")
	}
	if period.AllowanceBytes == 0 {
		return nil, errors.New("traffic allowance is required")
	}
	coverage := 1.0
	if period.Continuity != "complete" {
		coverage = 0.5
	}
	usage := (float64(period.CountedBytes) / float64(period.AllowanceBytes)) * 100
	if period.CountedBytes < period.AllowanceBytes && usage >= 100 {
		// uint64 counters can round a max-1 numerator to exactly 100% when
		// converted to float64; never fire an exact-100 threshold early.
		usage = math.Nextafter(100, 0)
	}
	return e.evaluateMetric(ctx, serverID, at, map[string]float64{"traffic.usage_percent": usage}, map[string]string{"traffic.usage_percent": validityForCoverage(coverage)}, coverage, "traffic", period.Scope, period.Direction)
}

func validityForCoverage(coverage float64) string {
	if coverage >= MinimumCoverage {
		return "valid"
	}
	return "uncertain"
}

func (e *Engine) evaluateMetric(ctx context.Context, serverID contracts.ServerID, at time.Time, values map[string]float64, validity map[string]string, coverage float64, metric string, trafficIdentity ...string) ([]Evaluation, error) {
	rules, err := e.listRules(ctx)
	if err != nil {
		return nil, err
	}
	results := make([]Evaluation, 0, len(rules))
	for _, rule := range rules {
		if rule.ServerID != "" && serverID != "" && rule.ServerID != serverID {
			continue
		}
		parsed, _, _, parseErr := ParseExpression(rule.Expression)
		if parseErr != nil || parsed != metric && !strings.HasPrefix(parsed, metric+".") {
			continue
		}
		if metric == "traffic" && len(trafficIdentity) == 2 && !trafficRuleMatchesAllowance(rule, trafficIdentity[0], trafficIdentity[1]) {
			continue
		}
		observationServer := serverID
		if observationServer == "" {
			observationServer = rule.ServerID
		}
		if observationServer == "" {
			// A global traffic rule needs a server-scoped period; do not create
			// an unscoped state that could merge different nodes.
			continue
		}
		result, evalErr := e.Evaluate(ctx, rule, Observation{ServerID: observationServer, ObservedAt: at, Values: values, Validity: validity, Coverage: coverage}, at)
		if evalErr != nil {
			return nil, evalErr
		}
		results = append(results, result)
	}
	return results, nil
}

func (e *Engine) listRules(ctx context.Context) ([]contracts.AlertRule, error) {
	// Paginate instead of silently ignoring rules after the first 200. The
	// storage page remains bounded; this loop is capped so a corrupt/unbounded
	// database cannot turn one observation into an unbounded evaluation.
	rules := make([]contracts.AlertRule, 0, monitoring.MaxPageItems)
	cursor := ""
	for pageNumber := 0; pageNumber < monitoring.MaxAlertRules/monitoring.MaxPageItems; pageNumber++ {
		page, err := e.Store.ListAlertRules(ctx, monitoring.MaxPageItems, cursor)
		if err != nil {
			return nil, err
		}
		rules = append(rules, page.Items...)
		if page.NextCursor == "" {
			return rules, nil
		}
		cursor = page.NextCursor
	}
	return rules, errors.New("alert rule count exceeds evaluation bound")
}

// StarterRules returns editable, enabled defaults for the core host signals.
// Owners may disable or tune each rule. Expressions are evaluated against
// timestamped samples; they do not imply continuous observations between
// sparse samples.
func StarterRules(serverID contracts.ServerID) []contracts.AlertRule {
	now := time.Now().UTC()
	base := []struct{ id, name, expression, group string }{
		{"cpu-high", "CPU above 90%", "cpu.utilization > 90", "cpu"},
		{"memory-low", "Available memory below 10%", "memory.used_percent > 90", "memory"},
		{"disk-high", "Disk above 90%", "disk.root.used_percent > 90", "disk"},
		{"inodes-high", "Inodes above 90%", "disk.root.inodes_used_percent > 90", "disk"},
		{"node-unreachable", "Node unreachable", "node.unreachable == 1", "availability"},
	}
	rules := make([]contracts.AlertRule, 0, len(base))
	for _, item := range base {
		identity := sha256.Sum256([]byte(item.id + "\x00" + string(serverID)))
		rules = append(rules, contracts.AlertRule{ID: item.id + "-" + hex.EncodeToString(identity[:6]), Name: item.name, Expression: item.expression, ServerID: serverID, Enabled: true, DurationSeconds: int64(DefaultAlertDuration / time.Second), ReminderSeconds: int64(DefaultReminder / time.Second), GroupKey: item.group, IdempotencyKey: "starter-" + hex.EncodeToString(identity[:12]), CreatedAt: now})
	}
	return rules
}

// TrafficAlertRules expands allowance warning percentages into editable rules.
// The default starter set contains the 90% rule; callers that configure a
// custom allowance can opt into the complete warning list without coupling
// alerting to any enforcement/control package.
func TrafficAlertRules(serverID contracts.ServerID, allowance contracts.TrafficAllowance) []contracts.AlertRule {
	allowance = trafficDefaults(allowance)
	if err := allowance.Validate(); err != nil {
		return nil
	}
	warnings := allowance.WarningPercentages
	if len(warnings) == 0 {
		warnings = []uint8{80, 90, 100}
	}
	rules := make([]contracts.AlertRule, 0, len(warnings))
	for _, threshold := range warnings {
		suffix := strings.TrimPrefix(trafficRuleID(serverID, allowance.Scope, allowance.Direction, float64(threshold)), "traffic-")
		rules = append(rules, contracts.AlertRule{ID: "traffic-" + suffix, Name: fmt.Sprintf("Traffic at %d%% allowance", threshold), Expression: "traffic.usage_percent >= " + strconv.Itoa(int(threshold)), ServerID: serverID, Enabled: true, DurationSeconds: int64(DefaultAlertDuration / time.Second), ReminderSeconds: int64(DefaultReminder / time.Second), GroupKey: trafficRuleGroup(allowance.Scope, allowance.Direction), IdempotencyKey: "traffic-starter-" + suffix, CreatedAt: time.Now().UTC()})
	}
	return rules
}

// trafficRuleGroup carries the allowance identity through the existing alert
// rule model. A traffic expression alone is not enough to distinguish, for
// example, a host allowance from an interface allowance on the same server.
func trafficRuleGroup(scope, direction string) string {
	sum := sha256.Sum256([]byte(scope + "\x00" + direction))
	return "traffic-" + hex.EncodeToString(sum[:8])
}

func trafficRuleID(serverID contracts.ServerID, scope, direction string, threshold float64) string {
	sum := sha256.Sum256([]byte(string(serverID) + "\x00" + scope + "\x00" + direction + "\x00" + strconv.FormatFloat(threshold, 'f', -1, 64)))
	return "traffic-" + hex.EncodeToString(sum[:8])
}

func trafficRuleMatchesAllowance(rule contracts.AlertRule, scope, direction string) bool {
	// The legacy per-server starter traffic rule predates allowance identity.
	// Configured allowances install scoped defaults, so evaluating this legacy
	// rule for every allowance would merge their state clocks.
	if strings.HasPrefix(rule.IdempotencyKey, "starter-") {
		return false
	}
	// The unscoped group is retained for owner-created traffic rules. Generated
	// defaults use the allowance identity group, so an owner may still edit the
	// expression/threshold without causing cross-allowance evaluation.
	if !strings.HasPrefix(rule.IdempotencyKey, "traffic-starter-") {
		return rule.GroupKey == "traffic"
	}
	return rule.GroupKey == trafficRuleGroup(scope, direction)
}

// trafficDefaults mirrors traffic.WithDefaults without importing the traffic
// package (which would create an alerts↔traffic dependency cycle).
func trafficDefaults(allowance contracts.TrafficAllowance) contracts.TrafficAllowance {
	if allowance.ResetDay == 0 {
		allowance.ResetDay = 1
	}
	if allowance.Timezone == "" {
		allowance.Timezone = "UTC"
	}
	if len(allowance.WarningPercentages) == 0 {
		allowance.WarningPercentages = []uint8{80, 90, 100}
	}
	return allowance
}

func (e *Engine) Evaluate(ctx context.Context, rule contracts.AlertRule, observation Observation, at time.Time) (Evaluation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := rule.Validate(); err != nil {
		return Evaluation{}, err
	}
	currentRule, current, err := e.Store.CurrentAlertRuleSnapshot(ctx, rule)
	if err != nil {
		return Evaluation{}, err
	}
	if !current {
		return Evaluation{}, monitoring.ErrAlertRuleChanged
	}
	// Direct callers created before effective_at was exposed may hold zero
	// timestamps. Resolve the durable boundary once, then carry it through the
	// transaction-guarded state write below.
	if rule.EffectiveAt.IsZero() {
		rule.EffectiveAt = currentRule.EffectiveAt
	}
	if !rule.Enabled {
		return Evaluation{Reason: "rule_disabled"}, nil
	}
	if _, _, _, err := ParseExpression(rule.Expression); err != nil {
		return Evaluation{}, err
	}
	if observation.ServerID == "" || (rule.ServerID != "" && rule.ServerID != observation.ServerID) || observation.ObservedAt.IsZero() {
		return Evaluation{}, errors.New("observation does not match alert rule")
	}
	if at.IsZero() {
		at = e.now()
	}
	at = at.UTC()
	observedAt := observation.ObservedAt.UTC()
	if at.Before(observedAt) {
		return Evaluation{}, errors.New("evaluation time precedes observation time")
	}
	// A durable sample may wait in the post-process queue while a rule is
	// created. Rule creation is the immutable effective boundary: historical
	// observations must not seed state clocks or emit retroactive events.
	effectiveAt := rule.EffectiveAt
	if effectiveAt.IsZero() {
		effectiveAt = rule.CreatedAt
	}
	if observedAt.Before(effectiveAt.UTC()) {
		return Evaluation{Reason: "observation_before_rule_effective_at"}, nil
	}
	stateID := alertStateID(rule.ID, observation.ServerID)
	state, found, err := e.Store.GetAlertState(ctx, stateID)
	if err != nil {
		return Evaluation{}, err
	}
	if !found {
		state = contracts.AlertState{ID: stateID, RuleID: rule.ID, ServerID: observation.ServerID, State: "recovered"}
	}
	previousObservation := state.LastObservation
	if previousObservation != nil && observation.ObservedAt.UTC().Before(previousObservation.UTC()) {
		return Evaluation{State: state, Reason: "out_of_order_observation"}, nil
	}
	state.LastObservation = timePtr(observedAt)
	value, condition, uncertain, reason := evaluateCondition(rule.Expression, observation, rule.RecoveryThreshold, state.State == "firing")
	if value != nil {
		state.LastValue = value
	}
	if uncertain {
		state.PrecisionWarning = reason
		// An uncertain/invalid observation cannot prove that a pending
		// condition remained continuously true. Keep the user-visible pending
		// state, but restart its evidence clock at this observation so a later
		// valid point cannot fire across an unobserved gap. A firing alert stays
		// firing until a valid recovery observation arrives.
		if state.State == "pending" {
			state.PendingSince = timePtr(observedAt)
			state.FiringSince = nil
		}
		if err := e.Store.SaveAlertStateForRule(ctx, rule, state); err != nil {
			return Evaluation{}, err
		}
		return Evaluation{State: state, Reason: reason}, nil
	}
	state.PrecisionWarning = ""
	previous := state.State
	lastNotifiedBefore := state.LastNotifiedAt
	suppressionActive, maintenanceErr := isMaintenance(ctx, e.Store, observation.ServerID, at)
	if maintenanceErr != nil {
		return Evaluation{}, maintenanceErr
	}
	if !suppressionActive && metricIsNotUnreachable(rule.Expression) {
		suppressionActive = e.isOutage(observation.ServerID)
		durableOutage, outageErr := e.Store.IsServerUnreachable(ctx, observation.ServerID)
		if outageErr != nil {
			return Evaluation{}, outageErr
		}
		suppressionActive = suppressionActive || durableOutage
	}
	sparse := previousObservation != nil && observedAt.Sub(previousObservation.UTC()) > time.Duration(rule.DurationSeconds)*time.Second
	if sparse {
		state.PrecisionWarning = "sampling_interval_exceeds_rule_duration"
	}
	if condition && sparse {
		// A sparse stream cannot prove the condition was continuously true for
		// the rule duration. Keep the state pending (or firing if it was already
		// established), but expose the precision warning and do not emit a new
		// firing/reminder notification from this isolated point.
		if state.State != "firing" {
			state.State = "pending"
			state.PendingSince = timePtr(observedAt)
			state.FiringSince = nil
		}
		if err := e.Store.SaveAlertStateForRule(ctx, rule, state); err != nil {
			return Evaluation{}, err
		}
		return Evaluation{State: state, Changed: previous != state.State, Reason: state.PrecisionWarning}, nil
	}
	changed := false
	notification := ""
	if state.State == "firing" {
		if !condition {
			state.State = "recovered"
			state.PendingSince = nil
			state.FiringSince = nil
			state.RecoveredAt = timePtr(observedAt)
			state.LastNotifiedAt = nil
			state.LastSuppressedAt = nil
			changed, notification = true, "recovered"
		} else if suppressionActive {
			// A zero reminder interval is the explicit, contract-supported
			// way to disable sustained reminders. It must not turn every
			// observation into a new durable event.
			if rule.ReminderSeconds > 0 && (state.LastSuppressedAt == nil || at.Sub(*state.LastSuppressedAt) >= time.Duration(rule.ReminderSeconds)*time.Second) {
				state.LastNotifiedAt = timePtr(at)
				notification = "reminder"
			}
		} else if state.LastSuppressedAt != nil {
			initialFiringWasSuppressed := state.LastNotifiedAt == nil
			state.LastSuppressedAt = nil
			if initialFiringWasSuppressed {
				// ReminderSeconds=0 disables only sustained reminders. It must not
				// permanently discard the initial firing notification merely because
				// that transition happened during maintenance or an outage.
				state.LastNotifiedAt = timePtr(at)
				notification = "firing"
			} else if rule.ReminderSeconds > 0 {
				state.LastNotifiedAt = timePtr(at)
				notification = "reminder"
			}
		} else if state.LastNotifiedAt == nil {
			// A firing state without a committed notification timestamp is a
			// failed/unaccepted initial delivery. Retry it independently of the
			// sustained-reminder setting (including ReminderSeconds=0).
			state.LastNotifiedAt = timePtr(at)
			notification = "firing"
		} else if rule.ReminderSeconds > 0 && at.Sub(*state.LastNotifiedAt) >= time.Duration(rule.ReminderSeconds)*time.Second {
			state.LastNotifiedAt = timePtr(at)
			notification = "reminder"
		}
	} else if condition {
		// A new condition supersedes any undelivered recovery from the previous
		// incident. A late recovery worker is guarded by state/incident checks.
		state.PendingRecoveryAt = nil
		if state.State != "pending" || state.PendingSince == nil {
			state.State = "pending"
			state.PendingSince = timePtr(observedAt)
			state.FiringSince = nil
			state.RecoveredAt = nil
			changed = true
		}
		if state.PendingSince != nil && observedAt.Sub(*state.PendingSince) >= time.Duration(rule.DurationSeconds)*time.Second {
			state.State = "firing"
			state.PendingSince = nil
			state.FiringSince = timePtr(observedAt)
			state.LastNotifiedAt = timePtr(at)
			changed, notification = true, "firing"
		}
	} else if state.State == "pending" {
		state.State = "recovered"
		state.PendingSince = nil
		state.FiringSince = nil
		state.RecoveredAt = timePtr(observedAt)
		changed = true
	} else if state.State == "recovered" && state.PendingRecoveryAt != nil {
		// Reuse the original recovery identity until delivery succeeds. This
		// survives restarts without producing a new history event per sample.
		notification = "recovered"
	}
	if !changed && notification != "" && e.deliveryReserved(deliveryReservationKey(state.ID, state.IncidentID, notification)) {
		if state.State == "firing" {
			state.LastNotifiedAt = lastNotifiedBefore
		}
		notification = ""
	}
	if changed || notification != "" {
		recoveryRetry := previous == "recovered" && notification == "recovered" && state.PendingRecoveryAt != nil
		eventAt := at
		if recoveryRetry {
			eventAt = state.PendingRecoveryAt.UTC()
		}
		eventState := state.State
		eventReason := reasonForTransition(previous, state.State, notification)
		if notification != "" && suppressionActive {
			eventState = "suppressed"
		}
		if notification != "" && state.State != "recovered" && metricIsNotUnreachable(rule.Expression) {
			outage := e.isOutage(observation.ServerID)
			durableOutage, outageErr := e.Store.IsServerUnreachable(ctx, observation.ServerID)
			if outageErr != nil {
				return Evaluation{}, outageErr
			}
			outage = outage || durableOutage
			if outage {
				eventState = "suppressed"
				eventReason = "server_unreachable"
			}
		}
		if notification != "recovered" && notification != "" && (state.IncidentID == "" || previous != "firing") {
			// Keep alerts in the same server/group/hour incident together, but
			// do not attach a later recurrence to a terminal incident forever.
			// A recovered alert can legitimately reopen within the grouping hour;
			// a later recurrence receives a fresh incident identity.
			desiredIncidentID := incidentID(rule, observation.ServerID, at)
			if state.IncidentID != desiredIncidentID {
				state.IncidentID = desiredIncidentID
			}
		}
		event := contracts.AlertHistoryEvent{ID: eventID(stateID, eventAt, eventState), AlertID: rule.ID, ServerID: observation.ServerID, State: eventState, OccurredAt: eventAt, Reason: eventReason, Value: value, IncidentID: state.IncidentID}
		suppressed := eventState == "suppressed"
		if suppressed && notification != "" {
			// Maintenance/outage suppression must not consume the reminder
			// clock; the first unsuppressed observation should notify promptly.
			state.LastNotifiedAt = lastNotifiedBefore
			state.LastSuppressedAt = timePtr(at)
		}
		var incident *contracts.IncidentSnapshot
		if notification != "" && state.State == "firing" {
			currentIncident, found, incidentErr := e.Store.GetIncident(ctx, state.IncidentID)
			if incidentErr != nil {
				return Evaluation{}, incidentErr
			}
			if found {
				// A group may recover and fire again within the deterministic
				// grouping window. Reopen the durable incident instead of leaving
				// the new firing attached to a terminal snapshot.
				if currentIncident.State != "open" {
					currentIncident.State = "open"
					currentIncident.EndedAt = nil
					currentIncident.StartedAt = at
				}
				var eventsTruncated bool
				currentIncident.Events, eventsTruncated = appendBoundedEvents(currentIncident.Events, event)
				currentIncident.Truncated = currentIncident.Truncated || eventsTruncated
				incident = &currentIncident
			} else {
				created := e.incidentForWithContext(ctx, rule, observation, state, at, event)
				incident = &created
			}
		} else if notification == "recovered" && state.IncidentID != "" && !recoveryRetry {
			currentIncident, found, incidentErr := e.Store.GetIncident(ctx, state.IncidentID)
			if incidentErr != nil {
				return Evaluation{}, incidentErr
			}
			if found {
				stillFiring, firingErr := e.Store.HasFiringAlertStateForIncident(ctx, state.IncidentID, stateID)
				if firingErr != nil {
					return Evaluation{}, firingErr
				}
				if !stillFiring {
					currentIncident.State = "recovered"
					currentIncident.EndedAt = timePtr(at)
				}
				var eventsTruncated bool
				currentIncident.Events, eventsTruncated = appendBoundedEvents(currentIncident.Events, event)
				currentIncident.Truncated = currentIncident.Truncated || eventsTruncated
				incident = &currentIncident
			}
		}
		notify := notification != "" && !suppressed
		deliveryQueue, deliveryDestinations := e.deliveryTargets(ctx, event.ServerID)
		deliveryConfigured := deliveryQueue != nil && len(deliveryDestinations) > 0
		deferCadence := notify && state.State == "firing" && deliveryConfigured
		if deferCadence {
			// Queue admission and terminal network delivery are both fallible.
			// Keep the durable clock at its previous value until at least one
			// destination succeeds, so a full/closed queue, process restart, or
			// exhausted retry policy cannot silently consume this cadence slot.
			state.LastNotifiedAt = lastNotifiedBefore
		}
		if notification == "recovered" && state.State == "recovered" && deliveryConfigured && state.PendingRecoveryAt == nil {
			state.PendingRecoveryAt = timePtr(eventAt)
		}
		reservationKey := ""
		if notify && deliveryConfigured {
			reservationKey = deliveryReservationKey(state.ID, state.IncidentID, notification)
			if !e.reserveDelivery(reservationKey) {
				notify = false
				reservationKey = ""
			}
		}
		if err := e.Store.SaveAlertEvaluationForRule(ctx, rule, state, &event, incident); err != nil {
			if reservationKey != "" {
				e.releaseDelivery(reservationKey)
			}
			return Evaluation{}, err
		}
		if notify && deliveryConfigured {
			notify = e.enqueue(deliveryQueue, deliveryDestinations, event, state, at, lastNotifiedBefore, reservationKey)
		} else if notify {
			// Preserve the state-machine behavior when delivery is not configured:
			// transition/reminder history remains cadence-bounded without inventing
			// a destination or persisting credentials.
			e.enqueue(deliveryQueue, deliveryDestinations, event, state, at, lastNotifiedBefore, "")
		}
		return Evaluation{State: state, Changed: changed, Notify: notify, Suppressed: suppressed, Notification: notification, Reason: event.Reason}, nil
	}
	if err := e.Store.SaveAlertStateForRule(ctx, rule, state); err != nil {
		return Evaluation{}, err
	}
	return Evaluation{State: state, Changed: changed}, nil
}

func (e *Engine) deliveryTargets(ctx context.Context, serverID contracts.ServerID) (*DeliveryQueue, []NotificationDestination) {
	// Evaluate owns e.mu for the whole state transition, so delivery
	// configuration cannot change while this snapshot is prepared.
	queue, provider := e.DeliveryQueue, e.Destinations
	if queue == nil || provider == nil {
		return queue, nil
	}
	destinations := provider(ctx, serverID)
	return queue, append([]NotificationDestination(nil), destinations...)
}

type deliveryAttempt struct {
	engine         *Engine
	reservationKey string
	state          contracts.AlertState
	deliveredAt    time.Time
	previous       *time.Time
	mu             sync.Mutex
	remaining      int
}

func (attempt *deliveryAttempt) complete(deliveryErr error) {
	if deliveryErr == nil {
		commitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if attempt.state.State == "firing" {
			_, _ = attempt.engine.Store.CommitAlertNotificationCadence(commitCtx, attempt.state.ID, attempt.state.IncidentID, attempt.previous, attempt.deliveredAt)
		} else if attempt.state.State == "recovered" && attempt.state.PendingRecoveryAt != nil {
			_, _ = attempt.engine.Store.CommitAlertRecoveryDelivery(commitCtx, attempt.state.ID, attempt.state.IncidentID, *attempt.state.PendingRecoveryAt)
		}
		cancel()
	}
	attempt.mu.Lock()
	attempt.remaining--
	done := attempt.remaining == 0
	attempt.mu.Unlock()
	if done {
		attempt.engine.releaseDelivery(attempt.reservationKey)
	}
}

func deliveryReservationKey(stateID, incidentID, notification string) string {
	class := "firing"
	if notification == "recovered" {
		class = "recovered"
	}
	return stateID + "\x00" + incidentID + "\x00" + class
}

func (e *Engine) deliveryReserved(key string) bool {
	e.deliveryMu.Lock()
	_, found := e.deliveries[key]
	e.deliveryMu.Unlock()
	return found
}

func (e *Engine) reserveDelivery(key string) bool {
	if key == "" {
		return false
	}
	e.deliveryMu.Lock()
	defer e.deliveryMu.Unlock()
	if e.deliveries == nil {
		e.deliveries = make(map[string]struct{})
	}
	if _, found := e.deliveries[key]; found || len(e.deliveries) >= maxDeliveryReservations {
		return false
	}
	e.deliveries[key] = struct{}{}
	return true
}

func (e *Engine) releaseDelivery(key string) {
	if key == "" {
		return
	}
	e.deliveryMu.Lock()
	delete(e.deliveries, key)
	e.deliveryMu.Unlock()
}

func (e *Engine) enqueue(queue *DeliveryQueue, destinations []NotificationDestination, event contracts.AlertHistoryEvent, state contracts.AlertState, deliveredAt time.Time, previous *time.Time, reservationKey string) bool {
	if queue == nil || len(destinations) == 0 {
		e.releaseDelivery(reservationKey)
		return false
	}
	attempt := &deliveryAttempt{engine: e, reservationKey: reservationKey, state: state, deliveredAt: deliveredAt, previous: previous, remaining: len(destinations)}
	accepted := false
	for _, destination := range destinations {
		err := queue.enqueue(context.Background(), destination, event, attempt.complete)
		if err == nil {
			accepted = true
		} else {
			attempt.complete(err)
		}
	}
	return accepted
}

func (e *Engine) isOutage(serverID contracts.ServerID) bool {
	e.outageMu.RLock()
	_, found := e.outages[serverID]
	e.outageMu.RUnlock()
	return found
}

func metricIsNotUnreachable(expression string) bool {
	metric, _, _, err := ParseExpression(expression)
	return err == nil && metric != "node.unreachable"
}

func evaluateCondition(expression string, observation Observation, recovery *float64, firing bool) (*float64, bool, bool, string) {
	metric, operator, threshold, err := ParseExpression(expression)
	if err != nil {
		return nil, false, true, "invalid_expression"
	}
	if observation.Coverage < MinimumCoverage || observation.Coverage > 1 || math.IsNaN(observation.Coverage) || math.IsInf(observation.Coverage, 0) {
		return nil, false, true, "coverage_is_too_sparse_for_alert_precision"
	}
	if metric == "node.unreachable" {
		value := 0.0
		if observation.Unreachable {
			value = 1
		}
		return &value, compare(value, operator, threshold), false, ""
	}
	state := observation.Validity[metric]
	if state != "" && state != "valid" {
		return nil, false, true, "metric_is_" + state
	}
	value, ok := observation.Values[metric]
	if !ok || math.IsNaN(value) || math.IsInf(value, 0) {
		return nil, false, true, "metric_is_unavailable"
	}
	if firing && recovery != nil {
		return &value, compare(value, operator, *recovery), false, ""
	}
	if firing {
		hysteresis := threshold
		if operator == ">" || operator == ">=" {
			// Move the recovery boundary away from the firing side by five
			// percent of the threshold magnitude. Multiplication alone is
			// directionally wrong for negative thresholds (for example,
			// `temperature < -10`).
			hysteresis = threshold - math.Abs(threshold)*0.05
		} else if operator == "<" || operator == "<=" {
			hysteresis = threshold + math.Abs(threshold)*0.05
		}
		return &value, compare(value, operator, hysteresis), false, ""
	}
	return &value, compare(value, operator, threshold), false, ""
}

func compare(value float64, operator string, threshold float64) bool {
	switch operator {
	case ">":
		return value > threshold
	case ">=":
		return value >= threshold
	case "<":
		return value < threshold
	case "<=":
		return value <= threshold
	case "==":
		return value == threshold
	case "!=":
		return value != threshold
	default:
		return false
	}
}

func reasonForTransition(previous, current, notification string) string {
	if notification == "reminder" {
		return "sustained_condition"
	}
	if notification == "firing" && previous == current {
		return "suppressed_initial_firing_released"
	}
	if previous == current {
		return "hysteresis_reminder"
	}
	return previous + "_to_" + current
}

func isMaintenance(ctx context.Context, store *monitoring.Store, serverID contracts.ServerID, at time.Time) (bool, error) {
	return store.IsMaintenance(ctx, serverID, at)
}

func incidentID(rule contracts.AlertRule, serverID contracts.ServerID, at time.Time) string {
	group := rule.GroupKey
	if group == "" {
		group = rule.ID
	}
	h := sha256.Sum256([]byte(string(serverID) + "\x00" + group + "\x00" + at.UTC().Format("2006-01-02T15")))
	return "incident-" + hex.EncodeToString(h[:12])
}

func alertStateID(ruleID string, serverID contracts.ServerID) string {
	h := sha256.Sum256([]byte(ruleID + "\x00" + string(serverID)))
	return "alert-" + hex.EncodeToString(h[:16])
}

func eventID(alertID string, at time.Time, state string) string {
	h := sha256.Sum256([]byte(alertID + "\x00" + state + "\x00" + at.UTC().Format(time.RFC3339Nano)))
	return "event-" + hex.EncodeToString(h[:16])
}

func timePtr(value time.Time) *time.Time { return &value }

func incidentFor(rule contracts.AlertRule, observation Observation, state contracts.AlertState, at time.Time, event contracts.AlertHistoryEvent) contracts.IncidentSnapshot {
	group := rule.GroupKey
	if group == "" {
		group = rule.ID
	}
	evidence := []string{fmt.Sprintf("metric observation %s at %s", rule.Expression, observation.ObservedAt.UTC().Format(time.RFC3339Nano))}
	return contracts.IncidentSnapshot{ID: state.IncidentID, ServerID: observation.ServerID, GroupKey: group, State: "open", StartedAt: at, Summary: "alert condition sustained; evidence is bounded and observational", Events: []contracts.AlertHistoryEvent{event}, Evidence: evidence}
}

// NotificationDestination is owner-configured delivery metadata. Secret is
// held only by the caller and is never serialized or included in errors.
type NotificationDestination struct {
	Kind    string
	URL     string
	Secret  string
	ChatID  string
	Enabled bool
}

type Notifier struct {
	Client *http.Client
}

func (n Notifier) Send(ctx context.Context, destination NotificationDestination, event contracts.AlertHistoryEvent) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// Bound the request even when a caller supplies an http.Client without a
	// timeout; the queue must not retain a worker forever on a stalled endpoint.
	requestContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if !destination.Enabled || destination.URL == "" || (destination.Kind != "webhook" && destination.Kind != "telegram") {
		return errors.New("notification destination is invalid or disabled")
	}
	if len(destination.Secret) > 256 {
		return errors.New("notification secret is too long")
	}
	if err := validateNotificationURL(destination.URL); err != nil {
		return err
	}
	payload := map[string]any{"alert_id": event.AlertID, "state": event.State, "occurred_at": event.OccurredAt.UTC().Format(time.RFC3339Nano), "reason": event.Reason}
	endpoint := destination.URL
	var body []byte
	if destination.Kind == "telegram" {
		if destination.Secret == "" || len(destination.Secret) > 256 || destination.ChatID == "" || len(destination.ChatID) > 128 {
			return errors.New("telegram destination token and chat id are required")
		}
		endpoint = strings.TrimRight(destination.URL, "/") + "/bot" + url.PathEscape(destination.Secret) + "/sendMessage"
		body, _ = json.Marshal(map[string]any{"chat_id": destination.ChatID, "text": fmt.Sprintf("Payesh alert %s: %s (%s)", event.AlertID, event.State, event.Reason)})
	} else {
		body, _ = json.Marshal(payload)
	}
	if len(body) > MaxNotificationBody {
		return errors.New("notification body exceeds limit")
	}
	client := n.Client
	if len(endpoint) > 2048 {
		return errors.New("notification endpoint is too long")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	} else {
		// A destination is validated only once, before the request. Do not let
		// net/http follow a redirect to a cleartext or otherwise unvalidated URL.
		// Clone the caller's client so this safety policy does not mutate shared
		// client state.
		clone := *client
		client = &clone
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("notification request could not be created")
	}
	request.Header.Set("Content-Type", "application/json")
	if destination.Kind == "webhook" && destination.Secret != "" {
		hash := hmac.New(sha256.New, []byte(destination.Secret))
		_, _ = hash.Write(body)
		request.Header.Set("X-Payesh-Signature", hex.EncodeToString(hash.Sum(nil)))
	}
	response, err := client.Do(request)
	if err != nil {
		// net/http wraps failures with the request URL. Telegram URLs contain
		// the bot token, so never return that wrapped error to callers/loggers.
		return errors.New("notification request failed")
	}
	defer response.Body.Close()
	if _, readErr := ioReadLimit(response.Body, MaxNotificationBody); readErr != nil {
		return errors.New("notification response exceeds limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("notification endpoint returned HTTP %d", response.StatusCode)
	}
	return nil
}

// SendWithRetry applies a bounded retry policy for transient notification
// outages. It never retries more than five times and caps waiting at 30s, so a
// failed destination cannot accumulate unbounded work or hold request memory.
func (n Notifier) SendWithRetry(ctx context.Context, destination NotificationDestination, event contracts.AlertHistoryEvent) error {
	if ctx == nil {
		ctx = context.Background()
	}
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<uint(attempt-1)) * time.Second
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		if err := n.Send(ctx, destination, event); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return lastErr
}

func validateNotificationURL(raw string) error {
	if len(raw) == 0 || len(raw) > 2048 {
		return errors.New("notification URL is outside its length bound")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("notification URL must be an HTTPS URL without credentials or fragments")
	}
	return nil
}

func appendBoundedEvents(events []contracts.AlertHistoryEvent, event contracts.AlertHistoryEvent) ([]contracts.AlertHistoryEvent, bool) {
	const maxEvents = 200
	truncated := false
	if len(events) >= maxEvents {
		events = append([]contracts.AlertHistoryEvent(nil), events[len(events)-maxEvents+1:]...)
		truncated = true
	}
	return append(events, event), truncated
}

func ioReadLimit(reader io.Reader, limit int64) ([]byte, error) {
	if limit < 0 {
		return nil, errors.New("invalid read limit")
	}
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("response exceeds limit")
	}
	return data, nil
}
