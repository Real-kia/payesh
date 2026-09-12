package bandwidth

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

const DefaultEnforcementInterval = 15 * time.Second

type Enforcer struct {
	Store    *monitoring.Store
	Manager  *Manager
	ServerID contracts.ServerID
	Interval time.Duration
	OnError  func(error)
}

func (e *Enforcer) Tick(ctx context.Context, now time.Time) error {
	if e.Store == nil || e.Manager == nil || e.ServerID == "" || now.IsZero() {
		return fmt.Errorf("bandwidth enforcer is not configured")
	}
	policies, err := e.Store.ListControlPolicies(ctx, e.ServerID, ModuleID)
	if err != nil {
		return err
	}
	for _, policy := range policies {
		if policy.State != contracts.ControlPolicyApplied {
			continue
		}
		var parameters PolicyParameters
		if err := json.Unmarshal(policy.Parameters, &parameters); err != nil {
			return fmt.Errorf("decode policy %s: %w", policy.ID, err)
		}
		if parameters.Request.QuotaBytes == 0 {
			continue
		}
		usageDirection := parameters.Request.UsageDirection
		if usageDirection == "" {
			usageDirection = string(parameters.Request.Scope.Direction)
		}
		period, found, err := e.Store.GetActiveTrafficPeriod(ctx, e.ServerID, parameters.Request.UsageScope, usageDirection, now)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		if !parameters.PeriodStart.IsZero() && !parameters.PeriodStart.Equal(period.From) && (parameters.EnforcementState == "active" || parameters.EnforcementState == "warned") {
			policy, err = e.Manager.ResetForNewPeriod(ctx, policy, parameters, period)
			if err != nil {
				return err
			}
			parameters.Checkpoint, parameters.EnforcementState, parameters.PeriodStart, parameters.CountedBytes = Checkpoint{}, "waiting", period.From, period.CountedBytes
		}
		if period.Continuity != "complete" {
			if _, err := e.Manager.MarkAccountingGap(ctx, policy, parameters, period); err != nil {
				return err
			}
			continue
		}
		if parameters.EnforcementState != "waiting" {
			continue
		}
		if _, err := e.Manager.ActivateDuePolicy(ctx, policy, parameters, period); err != nil {
			return err
		}
	}
	return nil
}

func (e *Enforcer) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	interval := e.Interval
	if interval == 0 {
		interval = DefaultEnforcementInterval
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
				if err := e.Tick(ctx, now.UTC()); err != nil && e.OnError != nil {
					e.OnError(err)
				}
			}
		}
	}()
	return done
}
