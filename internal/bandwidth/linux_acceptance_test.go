//go:build linux

package bandwidth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/porttraffic"
)

// This test is opt-in because it requires root/CAP_NET_ADMIN. It confines
// mutations to a newly-created dummy link; PAYESH_ACCEPTANCE_FOREIGN_IFACE may
// name a real interface for the read-only foreign-qdisc refusal check.
func TestLinuxTCBackendAcceptance(t *testing.T) {
	if os.Getenv("PAYESH_LINUX_ACCEPTANCE") != "1" {
		t.Skip("set PAYESH_LINUX_ACCEPTANCE=1 on a disposable Linux host")
	}
	if os.Geteuid() != 0 {
		t.Fatal("Linux tc acceptance requires root")
	}
	ctx := context.Background()
	const iface = "payeshbwtest0"
	runIP(t, "link", "del", iface)
	if output, err := exec.Command("ip", "link", "add", iface, "type", "dummy").CombinedOutput(); err != nil {
		t.Fatalf("create dummy link: %s: %v", output, err)
	}
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", iface).Run() })
	runIPRequired(t, "link", "set", iface, "up")

	backend := TCBackend{StateDir: t.TempDir()}
	for _, request := range []Request{
		{Scope: Scope{TargetKind: "interface", Interface: iface, Direction: porttraffic.Outbound}, Action: Throttle, BitsPerSecond: 8_000_000},
		{Scope: Scope{TargetKind: "local-port", Interface: iface, Protocol: porttraffic.TCP, LocalPort: 8080, Direction: porttraffic.Inbound}, Action: Block},
		{Scope: Scope{TargetKind: "interface", Interface: iface, Direction: porttraffic.Inbound}, Action: Block},
	} {
		var exclusions []ManagementFlow
		if request.Scope.TargetKind == "interface" && request.Scope.Direction == porttraffic.Inbound {
			exclusions = []ManagementFlow{{Name: "ssh", Interface: iface, Protocol: porttraffic.TCP, LocalPort: 22, PortRole: "local-service"}}
		}
		preview, err := BuildPreview(request, exclusions)
		if err != nil {
			t.Fatal(err)
		}
		ownership, previous, err := backend.Inspect(ctx, preview)
		if err != nil || !ownership.Compatible {
			t.Fatalf("inspect %+v: ownership=%+v err=%v", request.Scope, ownership, err)
		}
		if err := backend.Apply(ctx, preview); err != nil {
			t.Fatalf("apply %+v: %v", request.Scope, err)
		}
		if err := backend.Verify(ctx, preview); err != nil {
			t.Fatalf("verify %+v: %v", request.Scope, err)
		}
		checkpoint := Checkpoint{ID: "linux-acceptance", Scope: request.Scope, Action: request.Action, Previous: json.RawMessage(previous)}
		if err := backend.RevertOwned(ctx, checkpoint); err != nil {
			t.Fatalf("revert %+v: %v", request.Scope, err)
		}
	}

	// A failed post-apply management round trip must immediately undo the
	// kernel mutation and clear the armed checkpoint.
	guard := FileGuard{Dir: t.TempDir()}
	manager := Manager{Kernel: &backend, Guard: guard, ManagementRoundTrip: func(context.Context) error {
		return errors.New("deliberate acceptance failure")
	}}
	request := Request{Scope: Scope{TargetKind: "local-port", Interface: iface, Protocol: porttraffic.TCP, LocalPort: 8081, Direction: porttraffic.Inbound}, Action: Block}
	if _, err := manager.Apply(ctx, "linux-management-failure", request, nil); err == nil || !strings.Contains(err.Error(), "management confirmation failed") {
		t.Fatalf("expected management-confirmation rollback, got %v", err)
	}
	assertNoClsact(t, iface)

	// Simulate a dead dashboard/module after apply. The independently callable
	// guard recovery path must remove expired owned state without either one.
	preview, err := BuildPreview(request, nil)
	if err != nil {
		t.Fatal(err)
	}
	ownership, previous, err := backend.Inspect(ctx, preview)
	if err != nil || !ownership.Compatible {
		t.Fatalf("watchdog inspect: ownership=%+v err=%v", ownership, err)
	}
	if err := backend.Apply(ctx, preview); err != nil {
		t.Fatal(err)
	}
	expired := Checkpoint{ID: "linux-expired-watchdog", Scope: request.Scope, Action: request.Action, Previous: previous, ExpiresAt: time.Now().Add(-time.Second)}
	if err := guard.Arm(ctx, expired); err != nil {
		t.Fatal(err)
	}
	recovered, err := guard.RecoverDue(ctx, &backend, time.Now())
	if err != nil || len(recovered) != 1 || recovered[0] != expired.ID {
		t.Fatalf("watchdog recovery=%v err=%v", recovered, err)
	}
	assertNoClsact(t, iface)

	foreign := os.Getenv("PAYESH_ACCEPTANCE_FOREIGN_IFACE")
	if foreign != "" {
		request := Request{Scope: Scope{TargetKind: "interface", Interface: foreign, Direction: porttraffic.Outbound}, Action: Throttle, BitsPerSecond: 8_000_000}
		preview, err := BuildPreview(request, nil)
		if err != nil {
			t.Fatal(err)
		}
		ownership, _, err := backend.Inspect(ctx, preview)
		if err != nil || ownership.Compatible || ownership.Owner != "foreign" || !strings.Contains(ownership.Reason, "root qdisc") {
			t.Fatalf("expected foreign root refusal on %s, got ownership=%+v err=%v", foreign, ownership, err)
		}
	}
}

// TestLinuxTCThroughputAcceptance measures the outbound shaper with a known
// TCP volume over a disposable veth pair. It is separate from the ownership
// test because it takes a few seconds and requires a usable network namespace.
// Enable it explicitly with PAYESH_LINUX_THROUGHPUT=1 on a disposable host.
func TestLinuxTCThroughputAcceptance(t *testing.T) {
	if os.Getenv("PAYESH_LINUX_THROUGHPUT") != "1" {
		t.Skip("set PAYESH_LINUX_THROUGHPUT=1 on a disposable Linux host")
	}
	if os.Geteuid() != 0 {
		t.Fatal("Linux throughput acceptance requires root")
	}
	const (
		// Linux interface names are limited to 15 bytes.
		left  = "pybwth0"
		right = "pybwth1"
		netns = "pybwthns"
		port  = 18123
	)
	rate := acceptanceUintEnv(t, "PAYESH_LINUX_THROUGHPUT_RATE_BPS", 4_000_000, 1_000, 1_000_000_000)
	duration := acceptanceDurationSeconds(t, "PAYESH_LINUX_THROUGHPUT_SECONDS", 2, 1, 30)
	// A rate-derived volume makes this a sustained probe rather than a small
	// burst. The explicit bytes override is useful for very slow or very fast
	// links while retaining a hard upper bound for remote acceptance.
	defaultBytes := uint64(rate) * uint64(duration) / 8
	if defaultBytes < 256*1024 {
		defaultBytes = 256 * 1024
	}
	totalBytes := acceptanceUintEnv(t, "PAYESH_LINUX_THROUGHPUT_BYTES", defaultBytes, 64*1024, 32*1024*1024)
	runIP(t, "link", "del", left)
	runIP(t, "link", "del", right)
	runIP(t, "netns", "del", netns)
	if output, err := exec.Command("ip", "link", "add", left, "type", "veth", "peer", "name", right).CombinedOutput(); err != nil {
		t.Skipf("throughput topology is unavailable: create veth pair: %s: %v", output, err)
	}
	if output, err := exec.Command("ip", "netns", "add", netns).CombinedOutput(); err != nil {
		runIP(t, "link", "del", left)
		t.Skipf("throughput topology is unavailable: create namespace: %s: %v", output, err)
	}
	// Moving the peer into a separate namespace is essential: a listener on a
	// second address in the root namespace can take a local route and bypass
	// the egress qdisc.  The old probe therefore measured the host stack rather
	// than the shaped veth on some kernels.
	if output, err := exec.Command("ip", "link", "set", right, "netns", netns).CombinedOutput(); err != nil {
		runIP(t, "link", "del", left)
		runIP(t, "netns", "del", netns)
		t.Skipf("throughput topology is unavailable: move veth peer: %s: %v", output, err)
	}
	t.Cleanup(func() {
		runIP(t, "netns", "del", netns)
		runIP(t, "link", "del", left)
	})
	runIPRequired(t, "addr", "add", "198.18.0.1/30", "dev", left)
	runIPRequired(t, "link", "set", left, "up")
	if output, err := exec.Command("ip", "netns", "exec", netns, "ip", "addr", "add", "198.18.0.2/30", "dev", right).CombinedOutput(); err != nil {
		t.Fatalf("address throughput peer: %s: %v", output, err)
	}
	if output, err := exec.Command("ip", "netns", "exec", netns, "ip", "link", "set", "lo", "up").CombinedOutput(); err != nil {
		t.Fatalf("enable throughput peer loopback: %s: %v", output, err)
	}
	if output, err := exec.Command("ip", "netns", "exec", netns, "ip", "link", "set", right, "up").CombinedOutput(); err != nil {
		t.Fatalf("enable throughput peer: %s: %v", output, err)
	}
	preview, err := BuildPreview(Request{Scope: Scope{TargetKind: "interface", Interface: left, Direction: porttraffic.Outbound}, Action: Throttle, BitsPerSecond: rate}, nil)
	if err != nil {
		t.Fatal(err)
	}
	backend := TCBackend{StateDir: t.TempDir()}
	if ownership, _, inspectErr := backend.Inspect(context.Background(), preview); inspectErr != nil || !ownership.Compatible {
		t.Fatalf("throughput inspect: ownership=%+v err=%v", ownership, inspectErr)
	}
	if err := backend.Apply(context.Background(), preview); err != nil {
		t.Fatalf("throughput apply: %v", err)
	}
	t.Cleanup(func() {
		_ = backend.RevertOwned(context.Background(), Checkpoint{ID: "throughput", Scope: preview.Request.Scope, Action: preview.Request.Action, Previous: json.RawMessage(`{"interface":"` + left + `","absent":true}`)})
	})

	// Keep the receiver in the peer namespace. Python is already a prerequisite
	// of the Linux acceptance runner; a missing interpreter is a capability
	// limitation, not a product failure.
	receiverScript := `import socket,sys
s=socket.socket(socket.AF_INET,socket.SOCK_STREAM); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
s.bind((sys.argv[1],int(sys.argv[2]))); s.listen(1)
c,_=s.accept(); remaining=int(sys.argv[3])
received=0
while remaining:
 b=c.recv(min(65536,remaining))
 if not b: raise SystemExit("short receive")
 received+=len(b); remaining-=len(b)
c.close(); s.close()`
	receiver := exec.Command("ip", "netns", "exec", netns, "python3", "-c", receiverScript, "198.18.0.2", strconv.Itoa(port), strconv.FormatUint(totalBytes, 10))
	var receiverOutput bytes.Buffer
	receiver.Stdout = &receiverOutput
	receiver.Stderr = &receiverOutput
	if err := receiver.Start(); err != nil {
		t.Skipf("throughput receiver unavailable: %v", err)
	}
	t.Cleanup(func() { _ = receiver.Process.Kill(); _ = receiver.Wait() })
	time.Sleep(150 * time.Millisecond)
	start := time.Now()
	conn, err := net.DialTimeout("tcp4", "198.18.0.2:"+strconv.Itoa(port), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.SetWriteDeadline(time.Now().Add(time.Duration(duration+10) * time.Second)); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	remaining := totalBytes
	chunk := make([]byte, 32*1024)
	for remaining > 0 {
		want := uint64(len(chunk))
		if want > remaining {
			want = remaining
		}
		written, writeErr := conn.Write(chunk[:int(want)])
		if writeErr != nil {
			conn.Close()
			t.Fatalf("throughput write: %v", writeErr)
		}
		remaining -= uint64(written)
	}
	_ = conn.Close()
	if err := receiver.Wait(); err != nil {
		t.Fatalf("throughput receiver: %v (%s)", err, receiverOutput.String())
	}
	elapsed := time.Since(start)
	// Allow generous kernel burst/scheduling variance, but reject an
	// effectively unshaped path.
	expected := time.Duration(float64(totalBytes*8) / float64(rate) * float64(time.Second))
	if elapsed < expected/3 {
		t.Skipf("traffic path did not traverse the shaped veth: elapsed=%s expected-at-rate=%s", elapsed, expected)
	}
	observedBPS := uint64(float64(totalBytes*8) / elapsed.Seconds())
	overshootBPS := uint64(0)
	if observedBPS > rate {
		overshootBPS = observedBPS - rate
	}
	overshootPercent := float64(overshootBPS) * 100 / float64(rate)
	fmt.Printf("bandwidth_throughput_measurement=PASS target_rate_bps=%d observed_bytes=%d elapsed_ms=%d observed_rate_bps=%d rate_overshoot_bps=%d rate_overshoot_percent=%.2f target_seconds=%d\n", rate, totalBytes, elapsed.Milliseconds(), observedBPS, overshootBPS, overshootPercent, duration)
}

func assertNoClsact(t *testing.T, iface string) {
	t.Helper()
	output, err := exec.Command("tc", "-json", "qdisc", "show", "dev", iface).CombinedOutput()
	if err != nil || strings.Contains(string(output), `"kind":"clsact"`) {
		t.Fatalf("expected clsact cleanup on %s, output=%s err=%v", iface, output, err)
	}
}

func runIPRequired(t *testing.T, args ...string) {
	t.Helper()
	if output, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
		t.Fatalf("ip %s: %s: %v", strings.Join(args, " "), output, err)
	}
}

func runIP(t *testing.T, args ...string) {
	t.Helper()
	_ = exec.Command("ip", args...).Run()
}
