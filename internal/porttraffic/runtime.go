package porttraffic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

const DefaultRuntimeInterval = 15 * time.Second

// Backend is the narrow privileged boundary used by Runtime. Keeping it
// injectable makes reconciliation tests independent of a host nftables setup.
type Backend interface {
	Apply(context.Context, []Scope, string) error
	Remove(context.Context) error
	Snapshot(context.Context) ([]Counter, error)
}

type RuntimeOptions struct {
	Store    *monitoring.Store
	ServerID contracts.ServerID
	Backend  Backend
	Interval time.Duration
	OnError  func(error)
}

type Runtime struct {
	Store    *monitoring.Store
	ServerID contracts.ServerID
	Backend  Backend
	Interval time.Duration
	OnError  func(error)

	mu         sync.Mutex
	generation string
	tracker    *Tracker
}

func NewRuntime(options RuntimeOptions) (*Runtime, error) {
	if options.Store == nil || !validPortTrafficServer(options.ServerID) {
		return nil, errors.New("port traffic runtime options are incomplete")
	}
	backend := options.Backend
	if backend == nil {
		backend = NftBackend{}
	}
	interval := options.Interval
	if interval == 0 {
		interval = DefaultRuntimeInterval
	}
	if interval < time.Second {
		return nil, errors.New("port traffic runtime interval is too short")
	}
	return &Runtime{Store: options.Store, ServerID: options.ServerID, Backend: backend, Interval: interval, OnError: options.OnError, tracker: NewTracker()}, nil
}

// Tick reconciles the complete desired scope set in one nft transaction. A
// pending policy becomes applied only after the owned table has been accepted
// by nftables; a reverted/empty set removes only the owned Payesh table.
func (r *Runtime) Tick(ctx context.Context, now time.Time) error {
	if r == nil || r.Store == nil || r.Backend == nil || !validPortTrafficServer(r.ServerID) {
		return errors.New("port traffic runtime is not configured")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	policies, err := r.Store.ListControlPolicies(ctx, r.ServerID, ModuleID)
	if err != nil {
		return err
	}
	scopes := make([]Scope, 0, len(policies))
	pending := make([]contracts.ControlPolicy, 0)
	for _, policy := range policies {
		if policy.State != contracts.ControlPolicyPending && policy.State != contracts.ControlPolicyApplied {
			continue
		}
		var parameters ScopeParameters
		if err := json.Unmarshal(policy.Parameters, &parameters); err != nil {
			return fmt.Errorf("decode stored scope %s: %w", policy.ID, err)
		}
		if err := parameters.Scope.Validate(); err != nil {
			return fmt.Errorf("validate stored scope %s: %w", policy.ID, err)
		}
		if parameters.Scope.Path == "" {
			parameters.Scope.Path = LocalPath
		}
		scopes = append(scopes, parameters.Scope)
		if policy.State == contracts.ControlPolicyPending {
			pending = append(pending, policy)
		}
	}
	sort.Slice(scopes, func(i, j int) bool { return scopes[i].ID < scopes[j].ID })
	if len(scopes) == 0 {
		if err := r.Backend.Remove(ctx); err != nil {
			return err
		}
		r.generation = ""
		r.tracker = NewTracker()
		return nil
	}
	generation := scopesGeneration(scopes)
	if generation != r.generation {
		if err := r.Backend.Apply(ctx, scopes, generation); err != nil {
			return err
		}
		r.generation = generation
		r.tracker = NewTracker()
	}
	counters, err := r.Backend.Snapshot(ctx)
	if err != nil {
		return err
	}
	deltas, err := r.tracker.Observe(counters)
	if err != nil {
		return err
	}
	deltaByScope := make(map[string]Delta, len(deltas))
	for _, delta := range deltas {
		deltaByScope[delta.ScopeID] = delta
	}
	observations := make([]monitoring.PortTrafficObservation, 0, len(counters))
	for _, counter := range counters {
		delta := deltaByScope[counter.ScopeID]
		observations = append(observations, monitoring.PortTrafficObservation{ServerID: r.ServerID, ScopeID: counter.ScopeID, Bytes: counter.Bytes, Packets: counter.Packets, Generation: counter.Generation, ObservedAt: counter.ObservedAt, Continuity: delta.Continuity, Reason: delta.Reason})
	}
	if err := r.Store.UpsertPortTrafficObservations(ctx, observations); err != nil {
		return err
	}
	for _, policy := range pending {
		if _, err := r.Store.TransitionControlPolicy(ctx, r.ServerID, ModuleID, policy.TargetKind, policy.TargetName, policy.Revision, contracts.ControlPolicy{Kind: PolicyKind, State: contracts.ControlPolicyApplied, Parameters: policy.Parameters}); err != nil {
			return err
		}
	}
	_ = now // observations use the backend's authoritative sample timestamp.
	return nil
}

func (r *Runtime) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	interval := r.Interval
	if interval == 0 {
		interval = DefaultRuntimeInterval
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if err := r.Tick(ctx, now.UTC()); err != nil && r.OnError != nil {
					r.OnError(err)
				}
			}
		}
	}()
	return done
}

// Stop removes only Payesh's owned nftables table. It is safe to call during
// shutdown even if the table was already removed or never installed.
func (r *Runtime) Stop(ctx context.Context) error {
	if r == nil || r.Backend == nil {
		return errors.New("port traffic runtime is not configured")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	err := r.Backend.Remove(ctx)
	if err == nil {
		r.generation = ""
		r.tracker = NewTracker()
	}
	return err
}

func scopesGeneration(scopes []Scope) string {
	encoded, _ := json.Marshal(scopes)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:16])
}
