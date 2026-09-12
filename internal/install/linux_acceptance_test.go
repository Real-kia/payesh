//go:build linux

package install

import (
	"os"
	"strings"
	"testing"
)

// The supplied Ubuntu Focal host is intentionally outside the launch matrix.
// Keep this opt-in check as evidence that installation fails closed with a
// user-visible reason instead of treating an unsupported node as eligible.
func TestLinuxUnsupportedHostPreflight(t *testing.T) {
	if os.Getenv("PAYESH_LINUX_ACCEPTANCE") != "1" {
		t.Skip("set PAYESH_LINUX_ACCEPTANCE=1 on the supplied Linux host")
	}
	preflight, err := Check("/", "node", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("preflight=%s supported=%v problems=%v", Format(preflight), preflight.Supported, preflight.Problems)
	if preflight.Supported {
		t.Fatalf("Ubuntu Focal host unexpectedly passed the pinned launch matrix: %+v", preflight)
	}
	found := false
	for _, problem := range preflight.Problems {
		if strings.Contains(problem, "distribution is not in the launch support matrix") {
			found = true
		}
	}
	if !found {
		t.Fatalf("preflight did not explain unsupported distribution: %+v", preflight.Problems)
	}
}
