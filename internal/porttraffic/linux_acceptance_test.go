//go:build linux

package porttraffic

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

// This test is opt-in because it mutates the Payesh-owned nftables table and
// requires root/CAP_NET_ADMIN. It never flushes a ruleset or touches a foreign
// table; the table is removed before the test returns.
func TestLinuxNftBackendAcceptance(t *testing.T) {
	if os.Getenv("PAYESH_LINUX_ACCEPTANCE") != "1" {
		t.Skip("set PAYESH_LINUX_ACCEPTANCE=1 on a disposable Linux host")
	}
	if os.Geteuid() != 0 {
		t.Fatal("Linux nftables acceptance requires root")
	}
	if output, err := exec.Command("nft", "--version").CombinedOutput(); err != nil {
		t.Fatalf("nftables is unavailable: %s: %v", output, err)
	}
	ctx := context.Background()
	// Ubuntu Focal ships nftables 0.9.3, whose parser predates table/chain
	// comments. Payesh uses those comments as its ownership boundary; skip
	// cleanly and report the capability instead of applying unowned rules.
	probe := []byte("add table inet payesh_comment_probe { comment \"payesh\"; }\n")
	if result, err := (ExecRunner{}).Run(ctx, []string{"--check", "-f", "-"}, probe); err != nil {
		scopes := []Scope{
			{ID: "linux-in", Protocol: TCP, Interface: "lo", LocalPort: 8080, Direction: Inbound, Tuple: TranslatedTuple},
			{ID: "linux-out", Protocol: UDP, Interface: "lo", LocalPort: 5353, Direction: Outbound, Tuple: OriginalTuple},
		}
		if applyErr := (NftBackend{}).Apply(ctx, scopes, "linux-unsupported"); !errors.Is(applyErr, ErrNftablesUnsupported) {
			t.Fatalf("unsupported nftables syntax was not classified: %v", applyErr)
		}
		t.Skipf("nftables userspace lacks Payesh ownership-comment support: %s", result.Stderr)
	}
	backend := NftBackend{Now: func() time.Time { return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC) }}
	scopes := []Scope{
		{ID: "linux-in", Protocol: TCP, Interface: "lo", LocalPort: 8080, Direction: Inbound, Tuple: TranslatedTuple},
		{ID: "linux-out", Protocol: UDP, Interface: "lo", LocalPort: 5353, Direction: Outbound, Tuple: OriginalTuple},
		{ID: "linux-forwarded", Protocol: TCP, Interface: "lo", LocalPort: 8081, Direction: Inbound, Tuple: TranslatedTuple, Path: ForwardedPath},
	}
	exists, owned, err := backend.inspectOwnership(ctx)
	if err != nil {
		t.Fatal(err)
	} else if exists && !owned {
		if applyErr := backend.Apply(ctx, scopes, "linux-foreign"); !errors.Is(applyErr, ErrForeignTable) {
			t.Fatalf("foreign table was not refused safely: %v", applyErr)
		}
		t.Skip("Payesh Port Traffic table already exists without the exact ownership marker; refusing to mutate it")
	} else if owned && os.Getenv("PAYESH_ACCEPTANCE_REUSE_OWNED_PORT_TABLE") != "1" {
		t.Skip("Payesh Port Traffic table is already owned; set PAYESH_ACCEPTANCE_REUSE_OWNED_PORT_TABLE=1 to exercise replacement")
	}
	if err := backend.Apply(ctx, scopes, "linux-acceptance"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Remove(ctx) })
	counters, err := backend.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(counters) != len(scopes) {
		t.Fatalf("counter snapshot=%+v, want %d counters", counters, len(scopes))
	}
	seen := map[string]bool{}
	for _, counter := range counters {
		seen[counter.ScopeID] = true
		if counter.Generation != "linux-acceptance" || counter.Bytes != 0 || counter.Packets != 0 {
			t.Fatalf("unexpected counter=%+v", counter)
		}
	}
	if !seen["linux-in"] || !seen["linux-out"] || !seen["linux-forwarded"] {
		t.Fatalf("missing scope counters: %+v", seen)
	}
	if err := backend.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	if exists, owned, err := backend.inspectOwnership(ctx); err != nil {
		t.Fatal(err)
	} else if exists || owned {
		t.Fatalf("Payesh table survived removal: exists=%v owned=%v", exists, owned)
	}
}
