// Package contracts contains versioned wire-level values shared by Payesh
// artifacts. Feature behavior belongs to later packages; these types only
// establish stable names, units, and validation boundaries.
package contracts

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const ProtocolVersion = "v1"

const (
	NodeProtocol        = "payesh.node.v1"
	HelperProtocol      = "payesh.helper.v1"
	ModuleProtocol      = "payesh.module.v1"
	ReleaseFormat       = "payesh.release.v1"
	MaxEnvelopeBytes    = 1 << 20
	MaxBatchSamples     = 200
	MaxInstalledModules = 64
	MaxMetricFields     = 256
)

type ServerID string
type CollectorEpoch string

type Server struct {
	ID                    ServerID   `json:"id"`
	Name                  string     `json:"name"`
	Role                  string     `json:"role"`
	Architecture          string     `json:"architecture"`
	Platform              string     `json:"platform"`
	Capabilities          []string   `json:"capabilities"`
	Version               string     `json:"version,omitempty"`
	LastHeartbeat         *time.Time `json:"last_heartbeat,omitempty"`
	ConnectionState       string     `json:"connection_state"`
	FreshnessState        string     `json:"freshness_state"`
	FreshnessReason       string     `json:"freshness_reason,omitempty"`
	ConfigurationRevision uint64     `json:"configuration_revision,string"`
}

type Envelope struct {
	Protocol string          `json:"protocol"`
	Message  string          `json:"message"`
	Request  string          `json:"request_id,omitempty"`
	SentAt   time.Time       `json:"sent_at"`
	Body     json.RawMessage `json:"body,omitempty"`
}

// Node payloads are deliberately concrete so later consumers share one wire
// vocabulary instead of embedding incompatible JSON in Envelope.Body.
type Hello struct {
	Version               string            `json:"version"`
	ProtocolMin           string            `json:"protocol_min"`
	ProtocolMax           string            `json:"protocol_max"`
	Architecture          string            `json:"architecture"`
	Platform              string            `json:"platform"`
	Capabilities          []string          `json:"capabilities"`
	InstalledModules      []InstalledModule `json:"installed_modules,omitempty"`
	ConfigurationRevision uint64            `json:"configuration_revision,string"`
}

// InstalledModule is the bounded compatibility inventory a node presents in
// its hello message. It names installed artifacts; it does not enable them.
type InstalledModule struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

type Heartbeat struct {
	ServerID ServerID  `json:"server_id"`
	SentAt   time.Time `json:"sent_at"`
	Health   string    `json:"health"`
}

type SampleBatch struct {
	Samples []NodeMetricSample `json:"samples"`
	Gaps    []CoverageGap      `json:"gaps,omitempty"`
}

type Acknowledgement struct {
	RequestID       string `json:"request_id"`
	Accepted        bool   `json:"accepted"`
	ThroughSequence uint64 `json:"through_sequence,string"`
	Error           *Error `json:"error,omitempty"`
}

type Cancellation struct {
	JobID            string `json:"job_id"`
	IdempotencyKey   string `json:"idempotency_key"`
	ExpectedRevision uint64 `json:"expected_revision,string"`
}

func (e Envelope) Validate() error {
	if e.Protocol != NodeProtocol && e.Protocol != HelperProtocol && e.Protocol != ModuleProtocol {
		return errors.New("unsupported protocol")
	}
	if e.Message == "" || len(e.Message) > 64 {
		return errors.New("message must be 1..64 characters")
	}
	if e.SentAt.IsZero() {
		return errors.New("sent_at is required")
	}
	if len(e.Body) > MaxEnvelopeBytes {
		return errors.New("envelope body exceeds limit")
	}
	return nil
}

func (b SampleBatch) Validate() error {
	if len(b.Samples) > MaxBatchSamples || len(b.Gaps) > MaxBatchSamples {
		return errors.New("sample batch exceeds limit")
	}
	for _, sample := range b.Samples {
		if err := sample.Validate(); err != nil {
			return err
		}
	}
	for _, gap := range b.Gaps {
		if err := gap.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// NodeMetricSample is the node-to-hub wire shape. A node owns observed_at but
// can never authoritatively know received_at; the hub stamps that timestamp at
// durable ingestion via WithReceivedAt.
type NodeMetricSample struct {
	ServerID             ServerID           `json:"server_id"`
	CollectorEpoch       CollectorEpoch     `json:"collector_epoch"`
	Sequence             uint64             `json:"sequence,string"`
	ObservedAt           time.Time          `json:"observed_at"`
	TimestampUncertainty string             `json:"timestamp_uncertainty,omitempty"`
	Values               map[string]float64 `json:"values"`
	Counters             map[string]string  `json:"counters,omitempty"`
	Units                map[string]string  `json:"units,omitempty"`
	Validity             map[string]string  `json:"validity,omitempty"`
}

// WithReceivedAt creates the hub/API record. Callers must use the actual time
// at which the hub durably accepts the sample, not a node-provided value.
func (s NodeMetricSample) WithReceivedAt(receivedAt time.Time) (MetricSample, error) {
	if err := s.Validate(); err != nil {
		return MetricSample{}, err
	}
	if receivedAt.IsZero() {
		return MetricSample{}, errors.New("received_at is required at hub ingestion")
	}
	uncertainty := s.TimestampUncertainty
	if uncertainty == "" && receivedAt.Before(s.ObservedAt) {
		uncertainty = "source-clock-ahead"
	}
	return MetricSample{
		ServerID: s.ServerID, CollectorEpoch: s.CollectorEpoch, Sequence: s.Sequence,
		ObservedAt: s.ObservedAt, ReceivedAt: receivedAt,
		TimestampUncertainty: uncertainty, Values: s.Values,
		Counters: s.Counters, Units: s.Units, Validity: s.Validity,
	}, nil
}

type MetricSample struct {
	ServerID       ServerID       `json:"server_id"`
	CollectorEpoch CollectorEpoch `json:"collector_epoch"`
	Sequence       uint64         `json:"sequence,string"`
	ObservedAt     time.Time      `json:"observed_at"`
	ReceivedAt     time.Time      `json:"received_at"`
	// TimestampUncertainty records clock-skew interpretation without dropping
	// a sample whose source clock is ahead of the hub.
	TimestampUncertainty string             `json:"timestamp_uncertainty,omitempty"`
	Values               map[string]float64 `json:"values"`
	// Counters are decimal strings so byte and I/O totals remain exact when
	// consumed by JavaScript clients or cross a 53-bit integer boundary.
	Counters map[string]string `json:"counters,omitempty"`
	Units    map[string]string `json:"units,omitempty"`
	Validity map[string]string `json:"validity,omitempty"`
}

type Error struct {
	Code          string            `json:"code"`
	Message       string            `json:"message"`
	Retryable     bool              `json:"retryable"`
	CorrelationID string            `json:"correlation_id,omitempty"`
	Fields        map[string]string `json:"fields,omitempty"`
}

// AuditEvent is the redacted, durable record shared by enrollment, module,
// policy, update, and role-transition operations. Detail payloads must never
// contain credentials, private keys, tokens, or raw command output.
type AuditEvent struct {
	ID            string    `json:"id"`
	OccurredAt    time.Time `json:"occurred_at"`
	ActorType     string    `json:"actor_type"`
	ActorID       string    `json:"actor_id,omitempty"`
	Action        string    `json:"action"`
	TargetType    string    `json:"target_type"`
	TargetID      string    `json:"target_id,omitempty"`
	Result        string    `json:"result"`
	Revision      uint64    `json:"revision,string"`
	Redacted      bool      `json:"redacted"`
	CorrelationID string    `json:"correlation_id,omitempty"`
}

type JobRequest struct {
	JobID            string    `json:"job_id"`
	IdempotencyKey   string    `json:"idempotency_key"`
	ExpectedRevision uint64    `json:"expected_revision,string"`
	ExpiresAt        time.Time `json:"expires_at"`
}

type JobState string

const (
	JobQueued           JobState = "queued"
	JobRunning          JobState = "running"
	JobCancelling       JobState = "cancelling"
	JobSucceeded        JobState = "succeeded"
	JobFailed           JobState = "failed"
	JobCancelled        JobState = "cancelled"
	JobRecoveryRequired JobState = "recovery-required"
)

type Job struct {
	ID              string    `json:"id"`
	Kind            string    `json:"kind"`
	State           JobState  `json:"state"`
	Revision        uint64    `json:"revision,string"`
	IdempotencyKey  string    `json:"idempotency_key"`
	TargetServerID  ServerID  `json:"target_server_id,omitempty"`
	ExpiresAt       time.Time `json:"expires_at"`
	CancelRequested bool      `json:"cancel_requested,omitempty"`
	Progress        uint8     `json:"progress"`
	Error           *Error    `json:"error,omitempty"`
}

type CoverageGap struct {
	CollectorEpoch CollectorEpoch `json:"collector_epoch"`
	FromSequence   uint64         `json:"from_sequence,string"`
	ToSequence     uint64         `json:"to_sequence,string"`
	Reason         string         `json:"reason"`
}

// TrafficPeriod is a durable allowance/usage period. Calendar-boundary
// calculation belongs to the traffic consumer; storage preserves the
// explicit timezone and continuity so a month cannot be mistaken for an
// uninterrupted measurement window.
type TrafficPeriod struct {
	Scope          string    `json:"scope"`
	Interfaces     []string  `json:"interfaces,omitempty"`
	From           time.Time `json:"from"`
	To             time.Time `json:"to"`
	Timezone       string    `json:"timezone"`
	AllowanceBytes uint64    `json:"allowance_bytes,string"`
	Direction      string    `json:"direction"`
	CountedBytes   uint64    `json:"counted_bytes,string"`
	Continuity     string    `json:"continuity"`
}

// TrafficAllowance is the owner-selected accounting configuration for one
// server scope. Interfaces are names (not arbitrary paths) and are validated
// before an agent is allowed to use the configuration. ResetDay follows the
// calendar in Timezone; values 29..31 roll to the last day of shorter months.
type TrafficAllowance struct {
	Scope              string   `json:"scope"`
	Interfaces         []string `json:"interfaces,omitempty"`
	Direction          string   `json:"direction"`
	AllowanceBytes     uint64   `json:"allowance_bytes,string"`
	ResetDay           uint8    `json:"reset_day"`
	Timezone           string   `json:"timezone"`
	WarningPercentages []uint8  `json:"warning_percentages,omitempty"`
}

type TrafficObservation struct {
	ObservedAt    time.Time `json:"observed_at"`
	InboundBytes  uint64    `json:"inbound_bytes,string"`
	OutboundBytes uint64    `json:"outbound_bytes,string"`
	Valid         bool      `json:"valid"`
	Coverage      float64   `json:"coverage"`
}

type TrafficForecast struct {
	Available             bool      `json:"available"`
	Label                 string    `json:"label,omitempty"`
	Reason                string    `json:"reason,omitempty"`
	AsOf                  time.Time `json:"as_of"`
	PeriodEnd             time.Time `json:"period_end"`
	ObservedBytes         uint64    `json:"observed_bytes,string"`
	EstimatedBytesAtEnd   uint64    `json:"estimated_bytes_at_end,string"`
	EstimatedRemaining    uint64    `json:"estimated_remaining_bytes,string"`
	UsableDurationSeconds int64     `json:"usable_duration_seconds"`
	Coverage              float64   `json:"coverage"`
}

type AlertRule struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	Expression        string    `json:"expression"`
	ServerID          ServerID  `json:"server_id,omitempty"`
	Enabled           bool      `json:"enabled"`
	DurationSeconds   int64     `json:"duration_seconds"`
	RecoveryThreshold *float64  `json:"recovery_threshold,omitempty"`
	ReminderSeconds   int64     `json:"reminder_seconds"`
	GroupKey          string    `json:"group_key,omitempty"`
	IdempotencyKey    string    `json:"idempotency_key"`
	CreatedAt         time.Time `json:"created_at"`
	// EffectiveAt is the durable policy-version boundary. Queued observations
	// older than this instant cannot affect the current rule state.
	EffectiveAt time.Time `json:"effective_at"`
	// DisableReason is durable internal reconciliation metadata. It is kept
	// out of the public rule contract so an owner disable is distinguishable
	// from a generated-default threshold removal.
	DisableReason string `json:"-"`
}

type AlertState struct {
	ID               string     `json:"id"`
	Revision         uint64     `json:"-"`
	RuleID           string     `json:"rule_id"`
	ServerID         ServerID   `json:"server_id,omitempty"`
	State            string     `json:"state"`
	PendingSince     *time.Time `json:"pending_since,omitempty"`
	FiringSince      *time.Time `json:"firing_since,omitempty"`
	RecoveredAt      *time.Time `json:"recovered_at,omitempty"`
	LastObservation  *time.Time `json:"last_observation,omitempty"`
	LastValue        *float64   `json:"last_value,omitempty"`
	LastNotifiedAt   *time.Time `json:"last_notified_at,omitempty"`
	LastSuppressedAt *time.Time `json:"-"`
	// PendingRecoveryAt is internal durable delivery state. A non-nil value on
	// a recovered alert means its recovery notification has not yet reached a
	// configured destination and must be retried without duplicating history.
	PendingRecoveryAt *time.Time `json:"-"`
	IncidentID        string     `json:"incident_id,omitempty"`
	PrecisionWarning  string     `json:"precision_warning,omitempty"`
}

type AlertHistoryEvent struct {
	ID         string    `json:"id"`
	AlertID    string    `json:"alert_id"`
	ServerID   ServerID  `json:"server_id,omitempty"`
	State      string    `json:"state"`
	OccurredAt time.Time `json:"occurred_at"`
	Reason     string    `json:"reason,omitempty"`
	Value      *float64  `json:"value,omitempty"`
	IncidentID string    `json:"incident_id,omitempty"`
}

type MaintenanceWindow struct {
	ID             string     `json:"id"`
	IdempotencyKey string     `json:"idempotency_key"`
	StartsAt       time.Time  `json:"starts_at"`
	EndsAt         time.Time  `json:"ends_at"`
	ServerIDs      []ServerID `json:"server_ids,omitempty"`
	Reason         string     `json:"reason,omitempty"`
}

type IncidentSnapshot struct {
	ID        string              `json:"id"`
	ServerID  ServerID            `json:"server_id,omitempty"`
	GroupKey  string              `json:"group_key,omitempty"`
	State     string              `json:"state"`
	StartedAt time.Time           `json:"started_at"`
	EndedAt   *time.Time          `json:"ended_at,omitempty"`
	Summary   string              `json:"summary"`
	Events    []AlertHistoryEvent `json:"events,omitempty"`
	Evidence  []string            `json:"evidence"`
	Truncated bool                `json:"truncated,omitempty"`
}

func (p TrafficPeriod) Validate() error {
	if p.Scope == "" || len(p.Scope) > 128 || !metricNamePattern.MatchString(p.Scope) || p.From.IsZero() || p.To.IsZero() || !p.To.After(p.From) {
		return errors.New("traffic period identity and ordered bounds are required")
	}
	if p.Timezone == "" || len(p.Timezone) > 64 {
		return errors.New("traffic period timezone is required")
	}
	if _, err := time.LoadLocation(p.Timezone); err != nil {
		return errors.New("traffic period timezone is invalid")
	}
	if p.Direction != "inbound" && p.Direction != "outbound" && p.Direction != "combined" {
		return errors.New("traffic period direction is invalid")
	}
	if p.Continuity != "complete" && p.Continuity != "gap" && p.Continuity != "uncertain" {
		return errors.New("traffic period continuity is invalid")
	}
	return nil
}

func (a TrafficAllowance) Validate() error {
	if a.Scope == "" || len(a.Scope) > 128 || !metricNamePattern.MatchString(a.Scope) {
		return errors.New("traffic allowance scope is invalid")
	}
	if a.Direction != "inbound" && a.Direction != "outbound" && a.Direction != "combined" {
		return errors.New("traffic allowance direction is invalid")
	}
	if a.AllowanceBytes == 0 {
		return errors.New("traffic allowance must be greater than zero")
	}
	if a.ResetDay < 1 || a.ResetDay > 31 {
		return errors.New("traffic allowance reset day must be 1..31")
	}
	if a.Timezone == "" || len(a.Timezone) > 64 {
		return errors.New("traffic allowance timezone is required")
	}
	if _, err := time.LoadLocation(a.Timezone); err != nil {
		return errors.New("traffic allowance timezone is invalid")
	}
	if len(a.Interfaces) > 64 {
		return errors.New("traffic allowance interface list exceeds limit")
	}
	seen := make(map[string]struct{}, len(a.Interfaces))
	canonical := make(map[string]string, len(a.Interfaces))
	for _, iface := range a.Interfaces {
		if iface == "" || len(iface) > 64 || !metricNamePattern.MatchString(iface) {
			return errors.New("traffic allowance interface is invalid")
		}
		if iface == "billing" {
			return errors.New("traffic allowance interface billing is reserved")
		}
		if _, ok := seen[iface]; ok {
			return errors.New("traffic allowance interfaces must be unique")
		}
		seen[iface] = struct{}{}
		key := strings.NewReplacer(".", "_", ":", "_").Replace(iface)
		if prior, ok := canonical[key]; ok && prior != iface {
			return errors.New("traffic allowance interfaces collide after canonicalization")
		}
		canonical[key] = iface
	}
	if len(a.WarningPercentages) > 8 {
		return errors.New("traffic allowance warning list exceeds limit")
	}
	previous := uint8(0)
	for _, percentage := range a.WarningPercentages {
		if percentage == 0 || percentage > 100 || percentage <= previous {
			return errors.New("traffic allowance warnings must be increasing percentages")
		}
		previous = percentage
	}
	return nil
}

func (o TrafficObservation) Validate() error {
	if o.ObservedAt.IsZero() || !o.Valid || o.Coverage < 0 || o.Coverage > 1 || math.IsNaN(o.Coverage) || math.IsInf(o.Coverage, 0) {
		return errors.New("traffic observation is invalid")
	}
	return nil
}

func (r AlertRule) Validate() error {
	if r.ID == "" || len(r.ID) > 128 || !safeContractIdentifier(r.ID) || r.Name == "" || len(r.Name) > 128 || r.Expression == "" || len(r.Expression) > 1024 || r.IdempotencyKey == "" || len(r.IdempotencyKey) > 128 {
		return errors.New("alert rule identity and expression are invalid")
	}
	_, operator, threshold, expressionErr := parseAlertExpression(r.Expression)
	if expressionErr != nil {
		return expressionErr
	}
	if r.DurationSeconds < 1 || r.DurationSeconds > 24*60*60 || r.ReminderSeconds < 0 || r.ReminderSeconds > 7*24*60*60 {
		return errors.New("alert rule duration is outside the permitted bound")
	}
	if r.ServerID != "" && !serverIDPattern.MatchString(string(r.ServerID)) {
		return errors.New("alert rule server id is invalid")
	}
	if r.GroupKey != "" && (len(r.GroupKey) > 128 || !safeContractIdentifier(r.GroupKey)) {
		return errors.New("alert rule group is invalid")
	}
	if r.RecoveryThreshold != nil && (math.IsNaN(*r.RecoveryThreshold) || math.IsInf(*r.RecoveryThreshold, 0)) {
		return errors.New("alert rule recovery threshold is invalid")
	}
	if r.RecoveryThreshold != nil {
		recovery := *r.RecoveryThreshold
		valid := false
		switch operator {
		case ">", ">=":
			valid = recovery < threshold
		case "<", "<=":
			valid = recovery > threshold
		case "==", "!=":
			// Equality has no directional side. Requiring the same threshold
			// keeps the configured recovery predicate logically equivalent to
			// the original equality/inequality predicate rather than making a
			// firing alert switch to a different value.
			valid = recovery == threshold
		}
		if !valid {
			return errors.New("alert rule recovery threshold is not on the recovery side")
		}
	}
	if !r.CreatedAt.IsZero() && len(r.CreatedAt.Format(time.RFC3339Nano)) > 64 {
		return errors.New("alert rule timestamp is invalid")
	}
	if !r.EffectiveAt.IsZero() && len(r.EffectiveAt.Format(time.RFC3339Nano)) > 64 {
		return errors.New("alert rule effective timestamp is invalid")
	}
	return nil
}

func (s AlertState) Validate() error {
	if s.ID == "" || len(s.ID) > 128 || s.RuleID == "" || len(s.RuleID) > 128 {
		return errors.New("alert state identity is required")
	}
	if s.State != "pending" && s.State != "firing" && s.State != "recovered" {
		return errors.New("alert state is invalid")
	}
	if s.ServerID != "" && !serverIDPattern.MatchString(string(s.ServerID)) {
		return errors.New("alert state server id is invalid")
	}
	if s.PrecisionWarning != "" && len(s.PrecisionWarning) > 256 {
		return errors.New("alert state precision warning is too long")
	}
	if s.LastValue != nil && (math.IsNaN(*s.LastValue) || math.IsInf(*s.LastValue, 0)) {
		return errors.New("alert state value is invalid")
	}
	return nil
}

func (e AlertHistoryEvent) Validate() error {
	if e.ID == "" || len(e.ID) > 128 || e.AlertID == "" || len(e.AlertID) > 128 || e.OccurredAt.IsZero() {
		return errors.New("alert history identity and timestamp are required")
	}
	if e.State != "pending" && e.State != "firing" && e.State != "recovered" && e.State != "suppressed" {
		return errors.New("alert history state is invalid")
	}
	if len(e.Reason) > 512 {
		return errors.New("alert history reason is too long")
	}
	if e.Value != nil && (math.IsNaN(*e.Value) || math.IsInf(*e.Value, 0)) {
		return errors.New("alert history value is invalid")
	}
	if e.ServerID != "" && !serverIDPattern.MatchString(string(e.ServerID)) {
		return errors.New("alert history server id is invalid")
	}
	return nil
}

func (m MaintenanceWindow) Validate() error {
	if m.ID == "" || len(m.ID) > 128 || !safeContractIdentifier(m.ID) || m.IdempotencyKey == "" || len(m.IdempotencyKey) > 128 || m.StartsAt.IsZero() || m.EndsAt.IsZero() || !m.EndsAt.After(m.StartsAt) {
		return errors.New("maintenance window identity and ordered bounds are required")
	}
	if m.EndsAt.Sub(m.StartsAt) > 366*24*time.Hour || len(m.ServerIDs) > 20 || len(m.Reason) > 512 {
		return errors.New("maintenance window exceeds its bounds")
	}
	seen := make(map[ServerID]struct{}, len(m.ServerIDs))
	for _, serverID := range m.ServerIDs {
		if !serverIDPattern.MatchString(string(serverID)) {
			return errors.New("maintenance window server id is invalid")
		}
		if _, ok := seen[serverID]; ok {
			return errors.New("maintenance window server ids must be unique")
		}
		seen[serverID] = struct{}{}
	}
	return nil
}

func (i IncidentSnapshot) Validate() error {
	if i.ID == "" || len(i.ID) > 128 || !safeContractIdentifier(i.ID) || i.State != "open" && i.State != "recovered" || i.StartedAt.IsZero() || len(i.GroupKey) > 128 || len(i.Summary) > 512 || len(i.Events) > 200 || len(i.Evidence) > 200 {
		return errors.New("incident snapshot identity or state is invalid")
	}
	if i.ServerID != "" && !serverIDPattern.MatchString(string(i.ServerID)) {
		return errors.New("incident snapshot server id is invalid")
	}
	if i.EndedAt != nil && i.EndedAt.Before(i.StartedAt) {
		return errors.New("incident snapshot end must be after start")
	}
	for _, evidence := range i.Evidence {
		if len(evidence) > 4096 {
			return errors.New("incident evidence is too long")
		}
	}
	return nil
}

func (g CoverageGap) Validate() error {
	if g.CollectorEpoch == "" || len(g.CollectorEpoch) > 128 {
		return errors.New("collector_epoch is required")
	}
	if g.FromSequence >= g.ToSequence {
		return errors.New("coverage gap sequence bounds must be ordered")
	}
	if !map[string]bool{"spool-eviction": true, "transport-disconnect": true, "out-of-order": true, "retention": true}[g.Reason] {
		return errors.New("invalid coverage gap reason")
	}
	return nil
}

// ModuleState is the package-06 optional-module lifecycle. Install and
// activation are kept separate: a completed download/install lands on
// installed-disabled, never enabled, so a downloaded extra cannot silently
// begin enforcing anything.
type ModuleState string

const (
	ModuleUnavailable       ModuleState = "unavailable"
	ModuleAvailable         ModuleState = "available"
	ModuleDownloading       ModuleState = "downloading"
	ModuleVerifying         ModuleState = "verifying"
	ModuleInstalling        ModuleState = "installing"
	ModuleInstalledDisabled ModuleState = "installed-disabled"
	ModuleEnabled           ModuleState = "enabled"
	ModuleUpdating          ModuleState = "updating"
	ModuleRemoving          ModuleState = "removing"
	ModuleFailed            ModuleState = "failed"
)

const ModuleManifestFormat = "payesh.module-manifest.v1"

// ModuleManifest is the signed catalog description of one module release for
// one OS/architecture. It never carries a request-provided download URL;
// only the trusted catalog names eligible artifacts.
type ModuleManifest struct {
	Format               string    `json:"format"`
	ModuleID             string    `json:"module_id"`
	ModuleVersion        string    `json:"module_version"`
	MinCore              string    `json:"min_core"`
	MaxCore              string    `json:"max_core,omitempty"`
	ProtocolMin          string    `json:"protocol_min"`
	ProtocolMax          string    `json:"protocol_max"`
	OS                   string    `json:"os"`
	Architecture         string    `json:"architecture"`
	RequiredCapabilities []string  `json:"required_capabilities,omitempty"`
	Dependencies         []string  `json:"dependencies,omitempty"`
	RequiredPrivileges   []string  `json:"required_privileges,omitempty"`
	CompressedBytes      uint64    `json:"compressed_bytes,string"`
	UnpackedBytes        uint64    `json:"unpacked_bytes,string"`
	SHA256               string    `json:"sha256"`
	SigningKeyID         string    `json:"signing_key_id"`
	CreatedAt            time.Time `json:"created_at"`
}

// ModuleCatalogEntry is the curated, official-modules-only listing shown
// before any server-specific eligibility check. ResourceEstimateSource
// distinguishes a real release benchmark from an as-yet-unmeasured estimate;
// it must never be presented to an owner as a measured cost when unmeasured.
type ModuleCatalogEntry struct {
	ID                       string   `json:"id"`
	Name                     string   `json:"name"`
	Description              string   `json:"description"`
	LatestVersion            string   `json:"latest_version"`
	Dependencies             []string `json:"dependencies,omitempty"`
	RequiredPrivileges       []string `json:"required_privileges,omitempty"`
	EstimatedCompressedBytes uint64   `json:"estimated_compressed_bytes,string"`
	EstimatedUnpackedBytes   uint64   `json:"estimated_unpacked_bytes,string"`
	ResourceEstimateSource   string   `json:"resource_estimate_source"`
}

// ModuleInstallation is the durable per-server module lifecycle record.
type ModuleInstallation struct {
	ServerID  ServerID    `json:"server_id"`
	ModuleID  string      `json:"module_id"`
	Version   string      `json:"version,omitempty"`
	State     ModuleState `json:"state"`
	Revision  uint64      `json:"revision,string"`
	UpdatedAt time.Time   `json:"updated_at"`
	Error     *Error      `json:"error,omitempty"`
}

func (m ModuleManifest) Validate() error {
	if m.Format != ModuleManifestFormat {
		return errors.New("unsupported module manifest format")
	}
	if !safeContractIdentifier(m.ModuleID) {
		return errors.New("module_id is invalid")
	}
	if m.ModuleVersion == "" || len(m.ModuleVersion) > 64 {
		return errors.New("module_version is required")
	}
	if m.MinCore == "" {
		return errors.New("min_core is required")
	}
	if _, ok := parseProtocolVersion(m.ProtocolMin); !ok {
		return errors.New("protocol_min is invalid")
	}
	if _, ok := parseProtocolVersion(m.ProtocolMax); !ok {
		return errors.New("protocol_max is invalid")
	}
	if m.OS == "" || m.Architecture == "" {
		return errors.New("os and architecture are required")
	}
	if len(m.RequiredCapabilities) > 32 || len(m.Dependencies) > 32 || len(m.RequiredPrivileges) > 32 {
		return errors.New("module manifest inventory exceeds limit")
	}
	if m.CompressedBytes == 0 || m.UnpackedBytes == 0 {
		return errors.New("module manifest sizes are required")
	}
	if len(m.SHA256) != 64 {
		return errors.New("module manifest sha256 is invalid")
	}
	if m.SigningKeyID == "" || len(m.SigningKeyID) > 128 {
		return errors.New("signing_key_id is required")
	}
	if m.CreatedAt.IsZero() {
		return errors.New("created_at is required")
	}
	return nil
}

var moduleStates = map[ModuleState]bool{
	ModuleUnavailable: true, ModuleAvailable: true, ModuleDownloading: true,
	ModuleVerifying: true, ModuleInstalling: true, ModuleInstalledDisabled: true,
	ModuleEnabled: true, ModuleUpdating: true, ModuleRemoving: true, ModuleFailed: true,
}

func (i ModuleInstallation) Validate() error {
	if i.ServerID == "" {
		return errors.New("server_id is required")
	}
	if !safeContractIdentifier(i.ModuleID) {
		return errors.New("module_id is invalid")
	}
	if !moduleStates[i.State] {
		return errors.New("invalid module state")
	}
	if i.UpdatedAt.IsZero() {
		return errors.New("updated_at is required")
	}
	return nil
}

// ControlPolicyState is the durable lifecycle of one applied control
// (CPU/Bandwidth quota, and later similar controls). It is deliberately
// separate from ModuleState: installing a module never applies a policy, and
// a policy can be pending/applied/reverted/failed independently of whether
// its owning module later gets disabled.
type ControlPolicyState string

const (
	ControlPolicyPending  ControlPolicyState = "pending"
	ControlPolicyApplied  ControlPolicyState = "applied"
	ControlPolicyReverted ControlPolicyState = "reverted"
	ControlPolicyFailed   ControlPolicyState = "failed"
)

// ControlPolicy is the shared per-server, per-target control record listed
// in PLAN.md section 13's minimum data model. Parameters is a control-kind-
// specific typed payload (for example internal/cpucontrol's quota
// parameters), versioned and validated by the owning control package rather
// than by this shared envelope — the same pattern ModuleInvocationRequest
// uses for module-defined operation arguments.
type ControlPolicy struct {
	ID         string             `json:"id"`
	ServerID   ServerID           `json:"server_id"`
	ModuleID   string             `json:"module_id"`
	Kind       string             `json:"kind"`
	TargetKind string             `json:"target_kind"`
	TargetName string             `json:"target_name"`
	State      ControlPolicyState `json:"state"`
	Parameters json.RawMessage    `json:"parameters,omitempty"`
	Revision   uint64             `json:"revision,string"`
	UpdatedAt  time.Time          `json:"updated_at"`
	Error      *Error             `json:"error,omitempty"`
}

var controlPolicyStates = map[ControlPolicyState]bool{
	ControlPolicyPending: true, ControlPolicyApplied: true, ControlPolicyReverted: true, ControlPolicyFailed: true,
}

func (p ControlPolicy) Validate() error {
	if p.ServerID == "" {
		return errors.New("server_id is required")
	}
	if !safeContractIdentifier(p.ModuleID) || !safeContractIdentifier(p.Kind) {
		return errors.New("module_id and kind are invalid")
	}
	if p.TargetKind != "service" && p.TargetKind != "process-group" {
		return errors.New("target_kind must be service or process-group")
	}
	if p.TargetName == "" || len(p.TargetName) > 256 {
		return errors.New("target_name must be 1..256 characters")
	}
	if !controlPolicyStates[p.State] {
		return errors.New("invalid control policy state")
	}
	if len(p.Parameters) > MaxEnvelopeBytes {
		return errors.New("control policy parameters exceed the size limit")
	}
	if p.UpdatedAt.IsZero() {
		return errors.New("updated_at is required")
	}
	return nil
}

type FleetResult[T any] struct {
	Items     []T                `json:"items"`
	Succeeded []ServerID         `json:"succeeded"`
	Failed    map[ServerID]Error `json:"failed,omitempty"`
	Partial   bool               `json:"partial"`
}

// ActionRequest is the common shape for privileged, module, control, update,
// and role-transition jobs. The action is an enum owned by its consumer; the
// core never evaluates Arguments as a shell command.
type ActionRequest struct {
	Protocol         string   `json:"protocol"`
	RequestID        string   `json:"request_id"`
	Action           string   `json:"action"`
	TargetServerID   ServerID `json:"target_server_id,omitempty"`
	IdempotencyKey   string   `json:"idempotency_key"`
	ExpectedRevision uint64   `json:"expected_revision,string"`
	// Target is the local helper target (for example payesh-agent). It is
	// distinct from TargetServerID, which identifies a remote node.
	Target    string          `json:"target,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Deadline  time.Time       `json:"deadline"`
}

type ServiceActionArguments struct {
	Unit string `json:"unit,omitempty"`
}

type ArtifactActionArguments struct {
	Release string `json:"release,omitempty"`
	Path    string `json:"path,omitempty"`
}

type ModuleInvocationRequest struct {
	Protocol      string          `json:"protocol"`
	ModuleID      string          `json:"module_id"`
	ModuleVersion string          `json:"module_version"`
	RequestID     string          `json:"request_id"`
	Operation     string          `json:"operation"`
	Arguments     json.RawMessage `json:"arguments,omitempty"`
}

type ReleaseManifest struct {
	Format       string            `json:"format"`
	Release      string            `json:"release"`
	CreatedAt    time.Time         `json:"created_at"`
	MinCore      string            `json:"min_core"`
	Artifacts    []ReleaseArtifact `json:"artifacts"`
	SigningKeyID string            `json:"signing_key_id"`
}

type ReleaseArtifact struct {
	Name            string `json:"name"`
	OS              string `json:"os"`
	Arch            string `json:"arch"`
	SHA256          string `json:"sha256"`
	CompressedBytes uint64 `json:"compressed_bytes,string"`
	UnpackedBytes   uint64 `json:"unpacked_bytes,string"`
	URL             string `json:"url"`
}

type ActionResponse struct {
	RequestID string `json:"request_id"`
	Accepted  bool   `json:"accepted"`
	Revision  uint64 `json:"revision,string"`
	Error     *Error `json:"error,omitempty"`
}

var serverIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
var decimalPattern = regexp.MustCompile(`^[0-9]+$`)
var metricNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
var alertExpressionPattern = regexp.MustCompile(`^([A-Za-z0-9_.:-]{1,128})\s*(>=|<=|==|!=|>|<)\s*(-?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+))$`)
var protocolVersionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)$`)
var contractIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

func safeContractIdentifier(value string) bool {
	return contractIdentifierPattern.MatchString(value)
}

func parseAlertExpression(expression string) (string, string, float64, error) {
	matches := alertExpressionPattern.FindStringSubmatch(strings.TrimSpace(expression))
	if len(matches) != 4 {
		return "", "", 0, errors.New("alert rule expression is invalid")
	}
	threshold, err := strconv.ParseFloat(matches[3], 64)
	if err != nil || math.IsNaN(threshold) || math.IsInf(threshold, 0) {
		return "", "", 0, errors.New("alert rule expression threshold is invalid")
	}
	return matches[1], matches[2], threshold, nil
}

func (h Hello) Validate() error {
	if h.Version == "" || h.ProtocolMin == "" || h.ProtocolMax == "" {
		return errors.New("hello version and protocol range are required")
	}
	protocolMin, minOK := parseProtocolVersion(h.ProtocolMin)
	protocolMax, maxOK := parseProtocolVersion(h.ProtocolMax)
	supported, supportedOK := parseProtocolVersion(ProtocolVersion)
	if !minOK || !maxOK || !supportedOK || protocolMin > protocolMax || supported < protocolMin || supported > protocolMax {
		return errors.New("hello protocol range is incompatible")
	}
	if h.Architecture == "" || h.Platform == "" {
		return errors.New("hello architecture and platform are required")
	}
	if len(h.Capabilities) > 64 || len(h.InstalledModules) > MaxInstalledModules {
		return errors.New("hello inventory exceeds limit")
	}
	for _, module := range h.InstalledModules {
		if module.ID == "" || len(module.ID) > 128 || module.Version == "" || len(module.Version) > 64 {
			return errors.New("invalid installed module")
		}
	}
	return nil
}

func parseProtocolVersion(value string) (int, bool) {
	if !protocolVersionPattern.MatchString(value) {
		return 0, false
	}
	version, err := strconv.Atoi(value[1:])
	return version, err == nil
}

func (s MetricSample) Validate() error {
	if s.ReceivedAt.IsZero() {
		return errors.New("received_at is required for a hub metric sample")
	}
	return validateMetricFields(s.ServerID, s.CollectorEpoch, s.ObservedAt, s.TimestampUncertainty, s.Values, s.Counters, s.Units, s.Validity)
}

func (s NodeMetricSample) Validate() error {
	return validateMetricFields(s.ServerID, s.CollectorEpoch, s.ObservedAt, s.TimestampUncertainty, s.Values, s.Counters, s.Units, s.Validity)
}

func validateMetricFields(serverID ServerID, epoch CollectorEpoch, observedAt time.Time, uncertainty string, values map[string]float64, counters, units, validity map[string]string) error {
	if !serverIDPattern.MatchString(string(serverID)) {
		return errors.New("invalid server_id")
	}
	if epoch == "" {
		return errors.New("collector_epoch is required")
	}
	if observedAt.IsZero() {
		return errors.New("observed_at is required")
	}
	if values == nil {
		return errors.New("values object is required")
	}
	if len(values) > MaxMetricFields || len(counters) > MaxMetricFields || len(units) > MaxMetricFields || len(validity) > MaxMetricFields {
		return errors.New("metric field collection exceeds limit")
	}
	if uncertainty != "" && !map[string]bool{"none": true, "source-clock-ahead": true, "receipt-clock-ahead": true, "unknown": true}[uncertainty] {
		return errors.New("invalid timestamp_uncertainty")
	}
	for name, value := range values {
		if !metricNamePattern.MatchString(name) {
			return fmt.Errorf("metric %q has invalid name", name)
		}
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("metric %q is not finite", name)
		}
	}
	for name, value := range counters {
		if !metricNamePattern.MatchString(name) {
			return fmt.Errorf("counter %q has invalid name", name)
		}
		if !decimalPattern.MatchString(value) || (len(value) > 1 && value[0] == '0') {
			return fmt.Errorf("counter %q is not a decimal string", name)
		}
		if _, err := strconv.ParseUint(value, 10, 64); err != nil {
			return fmt.Errorf("counter %q is outside uint64 range", name)
		}
	}
	for name, state := range validity {
		if !metricNamePattern.MatchString(name) {
			return fmt.Errorf("metric %q has invalid name", name)
		}
		if !map[string]bool{"valid": true, "unavailable": true, "stale": true, "uncertain": true}[state] {
			return fmt.Errorf("metric %q has invalid validity", name)
		}
	}
	for name, unit := range units {
		if !metricNamePattern.MatchString(name) || len(unit) > 32 {
			return fmt.Errorf("metric %q has invalid unit", name)
		}
	}
	return nil
}

// ClockSkewCode gives consumers a stable uncertainty label when a node clock
// is ahead of the hub. Samples remain valid and retain both timestamps.
func (s MetricSample) ClockSkewCode() string {
	if s.TimestampUncertainty != "" {
		return s.TimestampUncertainty
	}
	if s.ReceivedAt.Before(s.ObservedAt) {
		return "source-clock-ahead"
	}
	return "none"
}

func (r JobRequest) Validate(now time.Time) error {
	if r.JobID == "" || len(r.JobID) > 128 {
		return errors.New("job_id must be 1..128 characters")
	}
	if r.IdempotencyKey == "" || len(r.IdempotencyKey) > 128 {
		return errors.New("idempotency_key must be 1..128 characters")
	}
	if r.ExpiresAt.IsZero() {
		return errors.New("expires_at is required")
	}
	if !r.ExpiresAt.After(now) {
		return errors.New("job has expired")
	}
	return nil
}

func (r ModuleInvocationRequest) Validate() error {
	if r.Protocol != ModuleProtocol {
		return errors.New("unsupported module protocol")
	}
	if r.ModuleID == "" || len(r.ModuleID) > 128 || r.ModuleVersion == "" || len(r.ModuleVersion) > 64 {
		return errors.New("module id and version are required")
	}
	if r.RequestID == "" || len(r.RequestID) > 128 || r.Operation == "" || len(r.Operation) > 128 {
		return errors.New("module request_id and operation are required")
	}
	return nil
}
