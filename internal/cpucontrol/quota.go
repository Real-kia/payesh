// Package cpucontrol implements the CPU Controls optional module: cgroup v2
// CPU quotas for an explicitly selected service or dedicated process group.
// It never limits "the whole server" — only the workloads an owner
// explicitly targets — and it never moves an unrelated process into a shared
// group or rewrites someone else's supervisor configuration.
package cpucontrol

import (
	"errors"
	"fmt"
)

// DefaultPeriodMicros is the cgroup v2 cpu.max accounting period (100ms),
// the conventional default the kernel documentation itself uses.
const DefaultPeriodMicros uint64 = 100000

// MaxMillicores bounds an accepted quota at 1000 cores. It exists only to
// reject an obviously malformed request (a typo adding stray digits); real
// systems rarely approach it.
const MaxMillicores uint64 = 1_000_000

// MillicoresPerCore is the API convention: 1000 millicores equal one core,
// matching Kubernetes' widely understood unit so "500m" reads the same way
// it does elsewhere. See PLAN.md section 10: "API values use integer
// millicores. Never confuse 100% of one core with 100% of the whole machine."
const MillicoresPerCore uint64 = 1000

// ValidateMillicores rejects zero, unset, and unreasonably large requests.
// Zero is rejected rather than silently treated as "unlimited": an explicit
// Unlimited flag exists at the Policy level for that case.
func ValidateMillicores(millicores uint64) error {
	if millicores == 0 {
		return errors.New("cpucontrol: millicores must be greater than zero")
	}
	if millicores > MaxMillicores {
		return fmt.Errorf("cpucontrol: millicores exceeds the supported maximum of %d", MaxMillicores)
	}
	return nil
}

// QuotaMicros converts an API millicore value to the cgroup v2 cpu.max quota
// value for DefaultPeriodMicros: quota/period == millicores/1000.
func QuotaMicros(millicores uint64, periodMicros uint64) uint64 {
	return millicores * periodMicros / MillicoresPerCore
}

// MillicoresFromQuota is QuotaMicros' inverse, used when reading an existing
// cpu.max back from the kernel (or a parent cgroup) to report an effective
// limit in the same API unit a caller requested one in.
func MillicoresFromQuota(quotaMicros, periodMicros uint64) uint64 {
	if periodMicros == 0 {
		return 0
	}
	return quotaMicros * 1000 / periodMicros
}

// Describe renders a human-readable explanation of a quota relative to the
// server's total core count, e.g. "1 core (1000m) — about 25% of this
// 4-vCPU server", matching the worked example in PLAN.md section 10.
func Describe(millicores uint64, totalCores int) string {
	cores := float64(millicores) / float64(MillicoresPerCore)
	if totalCores <= 0 {
		return fmt.Sprintf("%.2f core(s) (%dm)", cores, millicores)
	}
	percent := 100 * cores / float64(totalCores)
	return fmt.Sprintf("%.2f core(s) (%dm) — about %.0f%% of this %d-vCPU server", cores, millicores, percent, totalCores)
}
