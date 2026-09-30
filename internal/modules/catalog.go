// Package modules implements the package-06 signed optional-module
// framework: the curated catalog, per-server eligibility, the trust-verified
// install/enable/disable/remove lifecycle, and the safe archive staging that
// backs it. It never imports an optional module's own implementation; a
// module is only a signed manifest plus an archive the catalog approves.
package modules

import "github.com/Real-kia/payesh/internal/contracts"

// Catalog is the explicit, curated list of official Payesh modules. A
// general third-party executable marketplace is out of scope; only these IDs
// can ever reach an "available" state for a server.
//
// EstimatedCompressedBytes/EstimatedUnpackedBytes are placeholders pending a
// real release benchmark; ResourceEstimateSource records that honestly so
// the UI never presents a guess as a measurement.
var Catalog = []contracts.ModuleCatalogEntry{
	{
		ID:                       ProcessModuleID,
		Name:                     "Advanced Process Monitoring",
		Description:              "Per-process CPU, memory, disk I/O rates, and socket counts.",
		LatestVersion:            "0.1.0",
		EstimatedCompressedBytes: 1,
		EstimatedUnpackedBytes:   1,
		ResourceEstimateSource:   "unmeasured",
	},
	{
		ID:                       "port-traffic",
		Name:                     "Port Traffic",
		Description:              "Per-port TCP/UDP byte totals and rates using kernel counters.",
		LatestVersion:            "0.1.0",
		RequiredPrivileges:       []string{"nftables-counters"},
		EstimatedCompressedBytes: 1,
		EstimatedUnpackedBytes:   1,
		ResourceEstimateSource:   "unmeasured",
	},
	{
		ID:                       "cpu-controls",
		Name:                     "CPU Controls",
		Description:              "cgroup v2 CPU quotas for selected services and process groups.",
		LatestVersion:            "0.1.0",
		RequiredPrivileges:       []string{"cgroup-v2"},
		EstimatedCompressedBytes: 1,
		EstimatedUnpackedBytes:   1,
		ResourceEstimateSource:   "unmeasured",
	},
	{
		ID:            "bandwidth-controls",
		Name:          "Bandwidth Controls",
		Description:   "Interface/port speed caps and quota actions using Linux traffic control.",
		LatestVersion: "0.1.0",
		// Bandwidth Controls builds on the core traffic-accounting components
		// (package 05) rather than bundling another module; list that
		// dependency explicitly instead of silently pulling in extra code.
		Dependencies:             []string{"traffic-accounting"},
		RequiredPrivileges:       []string{"cgroup-v2", "tc", "nftables-counters"},
		EstimatedCompressedBytes: 1,
		EstimatedUnpackedBytes:   1,
		ResourceEstimateSource:   "unmeasured",
	},
}

// Lookup returns the catalog entry for id, if it is an official module.
func Lookup(id string) (contracts.ModuleCatalogEntry, bool) {
	for _, entry := range Catalog {
		if entry.ID == id {
			return entry, true
		}
	}
	return contracts.ModuleCatalogEntry{}, false
}

// Eligibility describes whether a specific server can install a module and,
// if not, exactly why. A missing dependency is reported by name rather than
// silently treated as satisfied.
type Eligibility struct {
	Eligible            bool     `json:"eligible"`
	MissingCapabilities []string `json:"missing_capabilities,omitempty"`
	UnsupportedPlatform bool     `json:"unsupported_platform,omitempty"`
	UnsupportedArch     bool     `json:"unsupported_arch,omitempty"`
	Reason              string   `json:"reason,omitempty"`
}

// CheckEligibility compares a server's advertised capabilities against a
// manifest's requirements and the catalog entry's core dependencies. It does
// not consult any privileged runtime state; a positive result still passes
// through real verification (signature, checksum, platform) during install.
func CheckEligibility(server contracts.Server, entry contracts.ModuleCatalogEntry, manifest contracts.ModuleManifest) Eligibility {
	result := Eligibility{Eligible: true}
	if manifest.OS != "" && manifest.OS != server.Platform {
		result.Eligible, result.UnsupportedPlatform = false, true
	}
	if manifest.Architecture != "" && manifest.Architecture != server.Architecture {
		result.Eligible, result.UnsupportedArch = false, true
	}
	have := make(map[string]bool, len(server.Capabilities))
	for _, capability := range server.Capabilities {
		have[capability] = true
	}
	missing := make([]string, 0)
	missingSet := make(map[string]struct{})
	addMissing := func(item string) {
		if _, exists := missingSet[item]; !exists {
			missing = append(missing, item)
			missingSet[item] = struct{}{}
		}
	}
	for _, dependency := range entry.Dependencies {
		if !have[dependency] {
			addMissing(dependency)
		}
	}
	for _, capability := range manifest.RequiredCapabilities {
		if !have[capability] {
			addMissing(capability)
		}
	}
	for _, privilege := range entry.RequiredPrivileges {
		if !have[privilege] {
			addMissing(privilege)
		}
	}
	for _, privilege := range manifest.RequiredPrivileges {
		if !have[privilege] {
			addMissing(privilege)
		}
	}
	if len(missing) > 0 {
		result.Eligible, result.MissingCapabilities = false, missing
	}
	switch {
	case result.UnsupportedPlatform:
		result.Reason = "server platform does not match this module release"
	case result.UnsupportedArch:
		result.Reason = "server architecture does not match this module release"
	case len(result.MissingCapabilities) > 0:
		result.Reason = "server is missing required capabilities/dependencies"
	}
	return result
}
