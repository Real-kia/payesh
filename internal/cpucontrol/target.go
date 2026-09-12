package cpucontrol

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ModuleID is this control's catalog identity, matching the entry
// internal/modules registers as "cpu-controls".
const ModuleID = "cpu-controls"

// PolicyKind identifies this control's ControlPolicy.Kind value, so the
// shared control_policies table can later hold Bandwidth Controls rows too
// without ambiguity about which control owns a given row.
const PolicyKind = "cpu-quota"

const (
	TargetKindService      = "service"
	TargetKindProcessGroup = "process-group"
)

var targetNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,254}$`)

// Target identifies one CPU-controllable workload. A service target names a
// systemd/OpenRC unit that already runs in — or can be given — its own
// dedicated cgroup. A process-group target requires Process to pin identity,
// so a reused PID can never silently inherit a stale policy.
type Target struct {
	Kind    string
	Name    string
	Process *ProcessIdentity
}

// Validate checks the addressable target fields without requiring a live
// process identity. Callers that apply a process-group policy should use the
// manager's identity-aware validation path instead.
func (t Target) Validate() error { return t.validateKindName() }

// validateIdentity is the full check Apply requires: a process-group target
// must carry a pinned process identity to verify before any quota is
// written.
func (t Target) validateIdentity() error {
	if err := t.validateKindName(); err != nil {
		return err
	}
	if t.Kind == TargetKindProcessGroup && t.Process == nil {
		return errors.New("cpucontrol: a process-group target requires a pinned process identity")
	}
	return nil
}

// validateKindName checks only the addressing fields. Revert and the
// disable-hook sweep restore whatever cgroup an already-persisted policy
// names; they do not need — and must not require — a live process identity
// for a workload that may since have exited.
func (t Target) validateKindName() error {
	if t.Kind != TargetKindService && t.Kind != TargetKindProcessGroup {
		return errors.New("cpucontrol: target kind must be service or process-group")
	}
	if !targetNamePattern.MatchString(t.Name) {
		return fmt.Errorf("cpucontrol: target name %q is invalid", t.Name)
	}
	return nil
}

// GroupPath computes the dedicated cgroup this target's policy lives in.
// Distinct kinds cannot collide because the kind prefixes the sanitized
// name.
func (t Target) GroupPath() GroupPath {
	// The readable component alone is not injective (':' and '_' both used to
	// become '_'). Include a digest of the original name so distinct API
	// targets can never alias the same kernel cgroup.
	digest := sha256.Sum256([]byte(t.Kind + "\x00" + t.Name))
	return GroupPath(fmt.Sprintf("payesh/%s-%s-%x", t.Kind, sanitizeGroupComponent(t.Name), digest[:8]))
}

func sanitizeGroupComponent(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
