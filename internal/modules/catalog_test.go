package modules

import (
	"testing"

	"github.com/Real-kia/payesh/internal/contracts"
)

func TestLookupOnlyReturnsCuratedModules(t *testing.T) {
	if _, ok := Lookup("port-traffic"); !ok {
		t.Fatal("expected port-traffic in the curated catalog")
	}
	if _, ok := Lookup("arbitrary-third-party-module"); ok {
		t.Fatal("catalog must never resolve an uncurated module id")
	}
}

func TestCheckEligibilityRejectsPlatformArchMismatch(t *testing.T) {
	server := contracts.Server{Architecture: "amd64", Platform: "linux", Capabilities: []string{"metrics"}}
	entry, _ := Lookup("port-traffic")
	manifest := contracts.ModuleManifest{OS: "linux", Architecture: "arm64"}
	result := CheckEligibility(server, entry, manifest)
	if result.Eligible || !result.UnsupportedArch {
		t.Fatalf("expected architecture mismatch to be rejected: %+v", result)
	}
}

func TestCheckEligibilityReportsMissingDependenciesAndCapabilities(t *testing.T) {
	server := contracts.Server{Architecture: "amd64", Platform: "linux", Capabilities: []string{"metrics"}}
	entry, _ := Lookup("bandwidth-controls")
	manifest := contracts.ModuleManifest{OS: "linux", Architecture: "amd64", RequiredCapabilities: []string{"cgroup-v2"}}
	result := CheckEligibility(server, entry, manifest)
	if result.Eligible {
		t.Fatal("expected missing traffic-accounting dependency and cgroup-v2 capability to fail eligibility")
	}
	want := map[string]bool{"traffic-accounting": true, "cgroup-v2": true, "tc": true, "nftables-counters": true}
	if len(result.MissingCapabilities) != len(want) {
		t.Fatalf("unexpected missing list: %v", result.MissingCapabilities)
	}
	for _, m := range result.MissingCapabilities {
		if !want[m] {
			t.Fatalf("unexpected missing entry %q", m)
		}
	}
}

func TestCheckEligibilityPassesWhenRequirementsAreSatisfied(t *testing.T) {
	server := contracts.Server{Architecture: "amd64", Platform: "linux", Capabilities: []string{"traffic-accounting", "cgroup-v2", "tc", "nftables-counters"}}
	entry, _ := Lookup("bandwidth-controls")
	manifest := contracts.ModuleManifest{OS: "linux", Architecture: "amd64"}
	result := CheckEligibility(server, entry, manifest)
	if !result.Eligible {
		t.Fatalf("expected eligibility to pass: %+v", result)
	}
}
