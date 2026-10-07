package transport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

var ErrAlreadyConnected = errors.New("node already has an active hub connection")
var ErrConnectionClosed = errors.New("connection_closed")
var ErrEnrollmentJobComplete = errors.New("enrollment_job_complete")
var ErrActionDeliveryUnsupported = errors.New("node does not advertise action delivery")
var actionPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

// Hub binds verified node identities to the existing durable monitoring
// ingestion seam. A websocket adapter must call Receive only after it has
// authenticated the TLS peer; acknowledgements are returned only after the
// store transaction succeeds.
type Hub struct {
	Ports       *NodePorts
	CA          *CertificateAuthority
	Store       *monitoring.Store
	mu          sync.Mutex
	connections map[contracts.ServerID]*Connection
	observerMu  sync.RWMutex
	observer    IngestionObserver
}

// IngestionObserver receives samples that were newly committed by the store,
// or an empty slice when a coverage-gap-only commit may unlock samples already
// waiting in the durable post-process queue. Implementations should keep their
// own durable idempotency boundary. It is a post-commit hook: a callback
// failure must not ask the node to resend already durable samples, because a
// duplicate batch will contain no newly inserted sample for the callback to
// process.
type IngestionObserver func(context.Context, []contracts.MetricSample) error

type Connection struct {
	hub                    *Hub
	stateMu                sync.RWMutex
	ServerID               contracts.ServerID
	hello                  contracts.Hello
	certificatePEM         []byte
	certificateFingerprint string
	closed                 bool
	jobs                   []contracts.ActionRequest
	seenJobKeys            map[string]struct{}
	seenJobOrder           []string
	leases                 map[string]monitoring.JobLease
}

func NewHub(ca *CertificateAuthority, store *monitoring.Store) (*Hub, error) {
	if ca == nil || store == nil {
		return nil, errors.New("certificate authority and store are required")
	}
	h := &Hub{CA: ca, Store: store, connections: make(map[contracts.ServerID]*Connection)}
	ca.addRevocationListener(h.handleRevocation)
	return h, nil
}

func (h *Hub) SetIngestionObserver(observer IngestionObserver) {
	if h == nil {
		return
	}
	h.observerMu.Lock()
	h.observer = observer
	h.observerMu.Unlock()
}

// LeaseNextJob claims one durable action for the currently authenticated
// connection. The connection check prevents a stale websocket from leasing
// work after a reconnect has taken ownership of the node identity.
func (h *Hub) LeaseNextJob(ctx context.Context, c *Connection, now time.Time) (monitoring.JobLease, bool, error) {
	if h == nil || c == nil || c.hub != h {
		return monitoring.JobLease{}, false, errors.New("connection does not belong to hub")
	}
	c.stateMu.RLock()
	closed := c.closed
	capabilities := append([]string(nil), c.hello.Capabilities...)
	serverID := c.ServerID
	configurationRevision := c.hello.ConfigurationRevision
	c.stateMu.RUnlock()
	if closed {
		return monitoring.JobLease{}, false, ErrConnectionClosed
	}
	if !supportsActionDelivery(capabilities) {
		return monitoring.JobLease{}, false, ErrActionDeliveryUnsupported
	}
	lease, ok, err := h.Store.LeaseNextActionJob(ctx, serverID, now.UTC(), 90*time.Second)
	if err != nil || !ok {
		return lease, ok, err
	}
	if lease.Action.ExpectedRevision != configurationRevision {
		_, completeErr := h.Store.CompleteActionJob(ctx, lease.Job.ID, lease.Token, contracts.ActionResponse{
			RequestID: lease.Action.RequestID,
			Accepted:  false,
			Error:     &contracts.Error{Code: "configuration_revision_conflict", Message: "node configuration changed before delivery", Retryable: false},
		}, now.UTC())
		if completeErr != nil {
			return monitoring.JobLease{}, false, completeErr
		}
		return monitoring.JobLease{}, false, nil
	}
	return lease, true, nil
}

// CreateActionJob is the server-side durable producer seam. Callers persist
// the complete typed request before a live connection is required; an active
// connection will discover it through LeaseNextJob and reconnects reclaim an
// expired lease safely.
func (h *Hub) CreateActionJob(ctx context.Context, job contracts.Job, request contracts.ActionRequest, requestHash string, now time.Time) (contracts.Job, bool, error) {
	if h == nil || h.Store == nil {
		return contracts.Job{}, false, errors.New("hub is not configured")
	}
	if err := request.Validate(now); err != nil || request.TargetServerID != job.TargetServerID || request.IdempotencyKey != job.IdempotencyKey {
		return contracts.Job{}, false, errors.New("invalid action job")
	}
	job.Action = &request
	return h.Store.CreateJob(ctx, job, requestHash, now)
}

func supportsActionDelivery(capabilities []string) bool {
	for _, capability := range capabilities {
		if capability == "actions" || capability == contracts.HelperProtocol || capability == contracts.ModuleProtocol {
			return true
		}
	}
	return false
}

// ConsumeEnrollment is the node bootstrap seam used before a client
// certificate exists. The short-lived pairing token is supplied by the node
// over its protected bootstrap channel; it is never persisted by the hub.
// The CA consumes it exactly once and the returned identity is the only place
// the new private key is exposed to the caller.
func (h *Hub) ConsumeEnrollment(ctx context.Context, token string, now time.Time) (NodeIdentity, error) {
	if h == nil || h.CA == nil || h.Store == nil {
		return NodeIdentity{}, errors.New("hub is not configured")
	}
	serverID, err := h.CA.EnrollmentServerID(token, now)
	if err != nil {
		return NodeIdentity{}, err
	}
	if _, found, err := h.Store.GetServer(ctx, serverID); err != nil {
		return NodeIdentity{}, err
	} else if !found {
		return NodeIdentity{}, errors.New("unenrolled_server")
	}
	identity, err := h.CA.ConsumeEnrollmentToken(token, now)
	if err != nil {
		return NodeIdentity{}, err
	}
	return identity, nil
}

// ConsumeEnrollmentJob claims a durable enrollment job and consumes the
// pairing token supplied by the node at claim time. The token and generated
// private key never enter SQLite. A successful claim returns the identity to
// the node and marks the job succeeded; invalid or expired claims become a
// durable failed job, so retries cannot silently replay work.
func (h *Hub) ConsumeEnrollmentJob(ctx context.Context, jobID string, token string, now time.Time) (NodeIdentity, contracts.Job, error) {
	if h == nil || h.Store == nil || h.CA == nil || jobID == "" || token == "" {
		return NodeIdentity{}, contracts.Job{}, errors.New("invalid enrollment job request")
	}
	job, found, err := h.Store.GetJob(ctx, jobID)
	if err != nil {
		return NodeIdentity{}, contracts.Job{}, err
	}
	if !found {
		return NodeIdentity{}, contracts.Job{}, monitoring.ErrJobNotFound
	}
	if job.Kind != "enrollment" || job.TargetServerID == "" {
		return NodeIdentity{}, job, monitoring.ErrEnrollmentJobInvalid
	}
	if job.State == contracts.JobSucceeded || job.State == contracts.JobFailed || job.State == contracts.JobCancelled || job.State == contracts.JobRecoveryRequired {
		return NodeIdentity{}, job, ErrEnrollmentJobComplete
	}
	tokenServerID, err := h.CA.EnrollmentServerID(token, now)
	if err != nil {
		return NodeIdentity{}, job, err
	}
	if tokenServerID != job.TargetServerID {
		return NodeIdentity{}, job, errors.New("enrollment token does not belong to job target")
	}
	claimed, err := h.Store.ClaimEnrollmentJob(ctx, job.ID, job.TargetServerID, job.Revision, now)
	if err != nil {
		return NodeIdentity{}, job, err
	}
	identity, err := h.ConsumeEnrollment(ctx, token, now)
	if err != nil {
		failed, transitionErr := h.Store.TransitionJob(ctx, claimed.ID, claimed.Revision, contracts.JobFailed, 100, &contracts.Error{Code: "enrollment_failed", Message: "enrollment bootstrap failed", Retryable: false}, now)
		if transitionErr != nil {
			return NodeIdentity{}, claimed, fmt.Errorf("enrollment failed: %v; recording failure: %w", err, transitionErr)
		}
		return NodeIdentity{}, failed, err
	}
	succeeded, transitionErr := h.Store.TransitionJob(ctx, claimed.ID, claimed.Revision, contracts.JobSucceeded, 100, nil, now)
	if transitionErr != nil {
		// The identity has already been issued and is returned to the node. The
		// caller can persist it even if the job acknowledgement needs recovery.
		return identity, claimed, fmt.Errorf("enrollment identity issued but job completion was not recorded: %w", transitionErr)
	}
	return identity, succeeded, nil
}

// Open verifies client identity, its protocol inventory, and that enrollment
// produced a known server. It deliberately rejects a second live connection
// for the same certificate identity instead of merging by name or IP.
func (h *Hub) Open(ctx context.Context, certificatePEM []byte, hello contracts.Hello, now time.Time) (*Connection, error) {
	if err := hello.Validate(); err != nil {
		return nil, err
	}
	id, err := h.CA.Verify(certificatePEM, now)
	if err != nil {
		return nil, err
	}
	certificateFingerprint, err := certificateFingerprintPEM(certificatePEM)
	if err != nil {
		return nil, err
	}
	if _, found, err := h.Store.GetServer(ctx, id); err != nil {
		return nil, err
	} else if !found {
		return nil, errors.New("unenrolled_server")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// Recheck while taking ownership of the connection. A revocation can race
	// the first verification; either the second check rejects it or the CA's
	// callback removes the just-created connection after this lock is released.
	if _, err := h.CA.Verify(certificatePEM, now); err != nil {
		return nil, err
	}
	if existing, exists := h.connections[id]; exists {
		// Revocation callbacks normally remove this entry immediately. The
		// extra check closes the small handoff window where a recovery Open can
		// race the callback and would otherwise see a stale one-live slot.
		if _, existingErr := h.CA.Verify(existing.certificatePEM, now); existingErr == nil {
			return nil, ErrAlreadyConnected
		}
		delete(h.connections, id)
		existing.stateMu.Lock()
		existing.closed = true
		existing.stateMu.Unlock()
	}
	c := &Connection{hub: h, ServerID: id, hello: hello, certificatePEM: append([]byte(nil), certificatePEM...), certificateFingerprint: certificateFingerprint, seenJobKeys: make(map[string]struct{}), leases: make(map[string]monitoring.JobLease)}
	h.connections[id] = c
	if err := h.Store.UpdateServerHello(ctx, id, hello, now.UTC()); err != nil {
		delete(h.connections, id)
		return nil, err
	}
	return c, nil
}

func (c *Connection) Close() {
	c.stateMu.Lock()
	if c.closed {
		c.stateMu.Unlock()
		return
	}
	c.closed = true
	c.stateMu.Unlock()
	c.detach()
}

// Renew rotates the certificate bound to this authenticated connection. The
// CA revokes the predecessor as part of the same durable transition; the
// websocket handler sends the returned identity before closing this socket so
// the node can reconnect with the replacement certificate.
func (c *Connection) Renew(ctx context.Context, now time.Time) (NodeIdentity, error) {
	if c == nil || c.hub == nil || c.hub.CA == nil || now.IsZero() {
		return NodeIdentity{}, errors.New("invalid renewal connection")
	}
	c.stateMu.RLock()
	if c.closed {
		c.stateMu.RUnlock()
		return NodeIdentity{}, ErrConnectionClosed
	}
	identity := NodeIdentity{ServerID: c.ServerID, CertificatePEM: append([]byte(nil), c.certificatePEM...), Fingerprint: c.certificateFingerprint}
	c.stateMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return NodeIdentity{}, err
	}
	// A hub that no longer owns the server must not extend its trust; the
	// store's own writes are fenced separately, this stops the CA acting alone.
	if c.hub.Store != nil {
		if err := c.hub.Store.RequireServerAuthority(ctx, c.ServerID); err != nil {
			return NodeIdentity{}, err
		}
	}
	return c.hub.CA.Renew(identity, now)
}

func (c *Connection) detach() {
	c.hub.mu.Lock()
	if current, ok := c.hub.connections[c.ServerID]; ok && current == c {
		delete(c.hub.connections, c.ServerID)
	}
	c.hub.mu.Unlock()
}

// handleRevocation is called by the CA after it records a revocation. It
// removes only the connection using the revoked certificate, so a recovery
// certificate for the same server cannot be mistaken for the old channel.
func (h *Hub) handleRevocation(serverID contracts.ServerID, certificateFingerprint string) {
	markRevoked := false
	h.mu.Lock()
	c, ok := h.connections[serverID]
	if !ok {
		// There is no replacement channel to protect, so retain the durable
		// revoked state even if the old connection had already disappeared.
		markRevoked = true
		c = nil
	} else if c.certificateFingerprint != certificateFingerprint {
		// A recovery certificate may have won the hand-off race. Do not mark
		// that new authenticated channel revoked merely because the callback for
		// the predecessor arrived afterward.
		c = nil
	} else {
		delete(h.connections, serverID)
		markRevoked = true
	}
	h.mu.Unlock()
	if markRevoked {
		// The durable state is updated even when the channel was already gone;
		// the dashboard must not present a revoked identity as merely
		// heartbeat-stale.
		_ = h.Store.MarkServerRevoked(context.Background(), serverID)
	}
	if c == nil {
		return
	}
	c.stateMu.Lock()
	c.closed = true
	c.stateMu.Unlock()
}

// invalidate closes a connection discovered to be expired or otherwise no
// longer trusted during an operation, and releases its one-live slot.
func (c *Connection) invalidate() {
	c.stateMu.Lock()
	if c.closed {
		c.stateMu.Unlock()
		return
	}
	c.closed = true
	c.stateMu.Unlock()
	c.detach()
}

func (c *Connection) ReceiveBatch(ctx context.Context, batch contracts.SampleBatch, now time.Time) (contracts.Acknowledgement, error) {
	c.stateMu.RLock()
	if c.closed {
		c.stateMu.RUnlock()
		return contracts.Acknowledgement{Accepted: false, Error: &contracts.Error{Code: "connection_closed", Message: "node connection is closed", Retryable: true}}, ErrConnectionClosed
	}
	if _, err := c.hub.CA.Verify(c.certificatePEM, now); err != nil {
		c.stateMu.RUnlock()
		c.invalidate()
		return contracts.Acknowledgement{Accepted: false, Error: &contracts.Error{Code: "connection_closed", Message: "node identity is no longer trusted", Retryable: true}}, ErrConnectionClosed
	}
	defer c.stateMu.RUnlock()
	if err := batch.Validate(); err != nil {
		return contracts.Acknowledgement{Accepted: false, Error: &contracts.Error{Code: "invalid_batch", Message: "invalid node measurement batch"}}, err
	}
	if len(batch.Samples) == 0 && len(batch.Gaps) == 0 {
		return contracts.Acknowledgement{Accepted: false, Error: &contracts.Error{Code: "invalid_batch", Message: "a batch must contain a sample or coverage gap"}}, errors.New("empty sample batch")
	}
	for _, sample := range batch.Samples {
		if sample.ServerID != c.ServerID {
			return contracts.Acknowledgement{Accepted: false, Error: &contracts.Error{Code: "identity_mismatch", Message: "sample identity does not match authenticated node"}}, errors.New("sample identity mismatch")
		}
	}
	if err := validateBatchEpoch(batch); err != nil {
		return contracts.Acknowledgement{Accepted: false, Error: &contracts.Error{Code: "invalid_batch", Message: "all samples and gaps must use one collector epoch"}}, err
	}
	result, err := c.hub.Store.IngestNodeBatch(ctx, c.ServerID, batch.Samples, batch.Gaps, now.UTC())
	if err != nil {
		return contracts.Acknowledgement{Accepted: false, Error: &contracts.Error{Code: "durable_ingest_failed", Message: "measurement was not accepted", Retryable: true}}, err
	}
	_ = result // Duplicate samples are also durably accepted.
	if err := c.hub.Store.TouchServer(ctx, c.ServerID, now.UTC()); err != nil {
		return contracts.Acknowledgement{Accepted: false, Error: &contracts.Error{Code: "heartbeat_failed", Message: "measurement accepted but freshness update failed", Retryable: true}}, err
	}
	c.hub.observerMu.RLock()
	observer := c.hub.observer
	c.hub.observerMu.RUnlock()
	if observer != nil && (len(result.InsertedSamples) > 0 || len(batch.Gaps) > 0) {
		// The raw sample is already durable. Do not return a retryable
		// acknowledgement here: a resend would be a duplicate and would not
		// provide the observer with the sample again. Store-level observers can
		// expose a pending result/metric and retry derived work independently.
		observerSamples := result.InsertedSamples
		if len(batch.Gaps) > 0 && observerSamples == nil {
			observerSamples = []contracts.MetricSample{}
		}
		_ = observer(ctx, observerSamples)
	}
	through := uint64(0)
	if len(batch.Samples) > 0 || len(batch.Gaps) > 0 {
		epoch := batchEpoch(batch)
		durableThrough, found, err := c.hub.Store.HighestContiguousCoverageSequence(ctx, c.ServerID, epoch)
		if err != nil {
			return contracts.Acknowledgement{Accepted: false, Error: &contracts.Error{Code: "acknowledgement_state_failed", Message: "measurement accepted but acknowledgement state could not be read", Retryable: true}}, err
		}
		if !found {
			// The batch was durably stored, but there is no safe cumulative
			// acknowledgement until sequence zero arrives. Returning an explicit
			// retryable gap keeps a node from evicting the unreceived prefix.
			return contracts.Acknowledgement{Accepted: false, Error: &contracts.Error{Code: "sequence_gap", Message: "sequence zero is not durably present; cumulative acknowledgement is withheld", Retryable: true}}, nil
		}
		through = durableThrough
	}
	return contracts.Acknowledgement{Accepted: true, ThroughSequence: through}, nil
}

func batchEpoch(batch contracts.SampleBatch) contracts.CollectorEpoch {
	if len(batch.Samples) > 0 {
		return batch.Samples[0].CollectorEpoch
	}
	if len(batch.Gaps) > 0 {
		return batch.Gaps[0].CollectorEpoch
	}
	return ""
}

// validateBatchEpoch keeps the acknowledgement unambiguous. ThroughSequence
// has no epoch field, so acknowledging a mixed-epoch batch could advance the
// wrong spool after an agent restart.
func validateBatchEpoch(batch contracts.SampleBatch) error {
	var epoch contracts.CollectorEpoch
	for _, sample := range batch.Samples {
		if epoch == "" {
			epoch = sample.CollectorEpoch
		} else if sample.CollectorEpoch != epoch {
			return errors.New("sample batch contains multiple collector epochs")
		}
	}
	for _, gap := range batch.Gaps {
		if epoch == "" {
			epoch = gap.CollectorEpoch
		} else if gap.CollectorEpoch != epoch {
			return errors.New("sample batch contains multiple collector epochs")
		}
	}
	return nil
}

// contiguousSampleSequence returns the highest sample sequence beginning at
// sequence zero that is present in this batch. A missing sequence is not
// acknowledged merely because a later sample was durably stored; the node may
// still have that missing sample in its spool. Coverage gaps remain durable
// records, but do not make an absent sample look accepted by ThroughSequence.
func contiguousSampleSequence(samples []contracts.NodeMetricSample) uint64 {
	if len(samples) == 0 {
		return 0
	}
	sequences := make([]uint64, 0, len(samples))
	seen := make(map[uint64]struct{}, len(samples))
	for _, sample := range samples {
		if _, ok := seen[sample.Sequence]; ok {
			continue
		}
		seen[sample.Sequence] = struct{}{}
		sequences = append(sequences, sample.Sequence)
	}
	sort.Slice(sequences, func(i, j int) bool { return sequences[i] < sequences[j] })
	through := uint64(0)
	for _, sequence := range sequences {
		if sequence != through {
			break
		}
		if through == ^uint64(0) {
			break
		}
		through++
	}
	if through == 0 {
		return 0
	}
	return through - 1
}

func (c *Connection) Heartbeat(ctx context.Context, heartbeat contracts.Heartbeat, now time.Time) error {
	c.stateMu.RLock()
	if c.closed {
		c.stateMu.RUnlock()
		return ErrConnectionClosed
	}
	if _, err := c.hub.CA.Verify(c.certificatePEM, now); err != nil {
		c.stateMu.RUnlock()
		c.invalidate()
		return ErrConnectionClosed
	}
	defer c.stateMu.RUnlock()
	if heartbeat.ServerID != c.ServerID || heartbeat.SentAt.IsZero() {
		return errors.New("heartbeat identity or time is invalid")
	}
	return c.hub.Store.TouchServer(ctx, c.ServerID, now.UTC())
}

// QueueJob only queues typed, unexpired work for the active authenticated
// node. The bounded FIFO prevents an unavailable client from consuming memory;
// a websocket writer can call NextJob with control messages prioritized.
func (c *Connection) QueueJob(request contracts.ActionRequest, now time.Time) error {
	if request.TargetServerID != c.ServerID || request.RequestID == "" || len(request.RequestID) > 128 || request.IdempotencyKey == "" || len(request.IdempotencyKey) > 128 || (request.Protocol != contracts.HelperProtocol && request.Protocol != contracts.ModuleProtocol) || !actionPattern.MatchString(request.Action) || request.Deadline.IsZero() || !request.Deadline.After(now) || len(request.Arguments) > contracts.MaxEnvelopeBytes {
		return errors.New("invalid_or_expired_job")
	}
	c.stateMu.Lock()
	if c.closed {
		c.stateMu.Unlock()
		return ErrConnectionClosed
	}
	if _, err := c.hub.CA.Verify(c.certificatePEM, now); err != nil {
		c.closed = true
		c.stateMu.Unlock()
		c.detach()
		return ErrConnectionClosed
	}
	if request.ExpectedRevision != c.hello.ConfigurationRevision {
		c.stateMu.Unlock()
		return errors.New("configuration_revision_conflict")
	}
	// Requests with an explicit local target are durable jobs. Keep the legacy
	// in-memory path below for the original low-level queue tests/callers that
	// intentionally omit a target; production action producers should always
	// provide one so reconnects cannot lose work.
	if request.Target != "" {
		data, err := json.Marshal(request)
		if err != nil {
			c.stateMu.Unlock()
			return err
		}
		hash := sha256.Sum256(data)
		jobID := "action-" + hex.EncodeToString(hash[:12])
		c.stateMu.Unlock()
		_, _, err = c.hub.Store.CreateJob(context.Background(), contracts.Job{ID: jobID, Kind: "action", State: contracts.JobQueued, IdempotencyKey: request.IdempotencyKey, TargetServerID: c.ServerID, ExpiresAt: request.Deadline, Action: &request}, hex.EncodeToString(hash[:]), now.UTC())
		return err
	}
	if len(c.jobs) >= 64 {
		c.stateMu.Unlock()
		return errors.New("job_queue_full")
	}
	for _, key := range []string{"request:" + request.RequestID, "idempotency:" + request.IdempotencyKey} {
		if _, exists := c.seenJobKeys[key]; exists {
			c.stateMu.Unlock()
			return errors.New("duplicate_job")
		}
	}
	if len(c.seenJobOrder) >= 4096 {
		// Keep replay protection bounded. A connection that has dispatched this
		// many distinct requests must reconnect before an old key can be reused.
		c.stateMu.Unlock()
		return errors.New("job_replay_window_full")
	}
	for _, key := range []string{"request:" + request.RequestID, "idempotency:" + request.IdempotencyKey} {
		c.seenJobKeys[key] = struct{}{}
		c.seenJobOrder = append(c.seenJobOrder, key)
	}
	c.jobs = append(c.jobs, request)
	c.stateMu.Unlock()
	return nil
}

// NextJob dequeues the next unexpired job. An optional timestamp is accepted
// for deterministic callers/tests; production callers may omit it and use the
// current UTC time. Expired jobs are discarded at dispatch time rather than
// being delivered after their deadline.
func (c *Connection) NextJob(at ...time.Time) (contracts.ActionRequest, bool) {
	now := time.Now().UTC()
	if len(at) > 0 && !at[0].IsZero() {
		now = at[0].UTC()
	}
	c.stateMu.Lock()
	if c.closed || len(c.jobs) == 0 {
		c.stateMu.Unlock()
		return contracts.ActionRequest{}, false
	}
	if _, err := c.hub.CA.Verify(c.certificatePEM, now); err != nil {
		c.closed = true
		c.stateMu.Unlock()
		c.detach()
		return contracts.ActionRequest{}, false
	}
	sort.SliceStable(c.jobs, func(i, j int) bool { return c.jobs[i].Deadline.Before(c.jobs[j].Deadline) })
	for len(c.jobs) > 0 {
		r := c.jobs[0]
		c.jobs = c.jobs[1:]
		if r.Deadline.After(now) {
			c.stateMu.Unlock()
			return r, true
		}
	}
	c.stateMu.Unlock()
	return contracts.ActionRequest{}, false
}
