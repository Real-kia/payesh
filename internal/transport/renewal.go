package transport

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"time"
)

const (
	DefaultCertificateValidity = 90 * 24 * time.Hour
	DefaultRenewBefore         = 30 * 24 * time.Hour
	DefaultRenewalCheck        = time.Hour
)

// RenewalScheduler owns the node-side renewal state machine. Renew and
// Recover are supplied by the authenticated transport. Recover must perform a
// fresh protected enrollment; it must never disable certificate verification
// or reuse an expired/revoked certificate. Persist is called before Identity
// is replaced so a restart cannot lose the only usable credential.
type RenewalScheduler struct {
	Identity    NodeIdentity
	RenewBefore time.Duration
	CheckEvery  time.Duration
	Now         func() time.Time
	Renew       func(context.Context, NodeIdentity) (NodeIdentity, error)
	Recover     func(context.Context, NodeIdentity, error) (NodeIdentity, error)
	Persist     func(NodeIdentity) error
}

func (s *RenewalScheduler) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *RenewalScheduler) due(now time.Time) bool {
	before := s.RenewBefore
	if before <= 0 {
		before = DefaultRenewBefore
	}
	expiresAt, _ := nodeIdentityExpiry(s.Identity)
	return !expiresAt.IsZero() && !now.Before(expiresAt.Add(-before))
}

// Check performs at most one renewal/recovery attempt. It is safe for a
// caller to invoke this from a bounded worker or a connection event loop.
func (s *RenewalScheduler) Check(ctx context.Context) error {
	if s == nil || s.Renew == nil || s.Identity.ServerID == "" {
		return ErrAgentNotConfigured
	}
	now := s.now()
	if _, err := nodeIdentityExpiry(s.Identity); err != nil {
		return fmt.Errorf("%w: current certificate expiry is unavailable: %v", ErrIdentityRecoveryRequired, err)
	}
	if !s.due(now) {
		return nil
	}
	next, err := s.Renew(ctx, s.Identity)
	if err != nil && s.Recover != nil {
		next, err = s.Recover(ctx, s.Identity, err)
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrIdentityRecoveryRequired, err)
	}
	if err := validateNodeIdentity(next, s.Identity.ServerID); err != nil {
		return fmt.Errorf("%w: invalid replacement identity: %v", ErrIdentityRecoveryRequired, err)
	}
	if s.Persist != nil {
		if err := s.Persist(next); err != nil {
			return fmt.Errorf("%w: persist replacement identity: %v", ErrIdentityRecoveryRequired, err)
		}
	}
	s.Identity = next
	return nil
}

func nodeIdentityExpiry(identity NodeIdentity) (time.Time, error) {
	if !identity.NotAfter.IsZero() {
		return identity.NotAfter, nil
	}
	block, _ := pem.Decode(identity.CertificatePEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return time.Time{}, fmt.Errorf("invalid node certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, err
	}
	return cert.NotAfter, nil
}

// Run checks immediately and then at a bounded cadence. A renewal or
// recovery failure stops the worker so the caller can surface the condition;
// it never loops rapidly on an expired or revoked identity.
func (s *RenewalScheduler) Run(ctx context.Context) error {
	if s == nil {
		return ErrAgentNotConfigured
	}
	if err := s.Check(ctx); err != nil {
		return err
	}
	interval := s.CheckEvery
	if interval <= 0 {
		interval = DefaultRenewalCheck
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.Check(ctx); err != nil {
				return err
			}
		}
	}
}
