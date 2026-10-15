package main

import (
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// The agent runs on every node and should stay small and free of hub-only
// code (fleet API, accounts, alert evaluation, geolocation, MCP, TLS
// management). This is a ratchet: it lists exactly the internal packages the
// agent links today. A new dependency fails the test; one that is no longer
// linked must be removed from the list so the boundary only tightens.
//
// The hub side of the node transport (Hub, certificate-bound connections,
// port migration) lives in internal/nodehub and must stay out of this list.
//
// Remaining debt: auth, monitoring and dbschema (and with them the SQLite
// driver) are linked through webupdate -> updater and through two helpers the
// agent calls in monitoring. Splitting webupdate into the part the agent calls
// and the root worker, and moving those helpers into small packages, would let
// all of them go.
var allowedAgentPackages = map[string]bool{
	"collector":  true,
	"contracts":  true,
	"dbschema":   true, // via monitoring and updater
	"auth":       true, // via monitoring
	"monitoring": true, // via updater and two agent helpers
	"netproxy":   true,
	"privd":      true,
	"transport":  true,
	"trust":      true,
	"updater":    true,
	"version":    true,
	"webupdate":  true,
}

func TestAgentLinksNoUnexpectedInternalPackages(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	const prefix = "github.com/Real-kia/payesh/internal/"
	linked := map[string]bool{}
	for _, line := range strings.Fields(string(out)) {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			linked[strings.SplitN(rest, "/", 2)[0]] = true
		}
	}
	var unexpected, stale []string
	for name := range linked {
		if !allowedAgentPackages[name] {
			unexpected = append(unexpected, name)
		}
	}
	for name := range allowedAgentPackages {
		if !linked[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(unexpected)
	sort.Strings(stale)
	if len(unexpected) > 0 {
		t.Errorf("the node agent now links hub-side packages %v; keep hub code out of the agent or move the shared part into a smaller package", unexpected)
	}
	if len(stale) > 0 {
		t.Errorf("the node agent no longer links %v; remove them from allowedAgentPackages", stale)
	}
}
