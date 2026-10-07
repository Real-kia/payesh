//go:build linux

package porttraffic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"testing"
	"time"
)

// Actual IPv4/UDP kernel accounting, separate from provider billing and
// forwarded/NAT/container coverage. Run only through the acceptance namespace
// wrapper; no existing table is adopted, replaced, or removed.
func TestLinuxNftUDPAccountingAcceptance(t *testing.T) {
	if os.Getenv("PAYESH_LINUX_ACCOUNTING") != "1" {
		t.Skip("set PAYESH_LINUX_ACCOUNTING=1 in an isolated acceptance namespace")
	}
	namespace, err := os.Readlink("/proc/self/ns/net")
	if err != nil || os.Geteuid() != 0 || os.Getenv("PAYESH_ACCEPTANCE_OUTER_NETNS") == "" || namespace != os.Getenv("PAYESH_ACCEPTANCE_OUTER_NETNS") {
		t.Fatal("requires root and the isolated network acceptance wrapper")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	backend := NftBackend{}
	exists, _, err := backend.inspectOwnership(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("accounting fixture refuses any preexisting Port Traffic table")
	}
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := net.DialUDP("udp4", nil, server.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	port := uint16(server.LocalAddr().(*net.UDPAddr).Port)
	scopes := []Scope{{ID: "udp-in", Protocol: UDP, Interface: "lo", LocalPort: port, Direction: Inbound, Tuple: TranslatedTuple}, {ID: "udp-out", Protocol: UDP, Interface: "lo", LocalPort: port, Direction: Outbound, Tuple: TranslatedTuple}}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := backend.Remove(cleanup); err != nil {
			t.Errorf("remove isolated fixture table: %v", err)
		}
	})
	if err := backend.Apply(ctx, scopes, "udp-generation-1"); err != nil {
		if errors.Is(err, ErrNftablesUnsupported) {
			t.Skipf("required nft ownership metadata unsupported: %v", err)
		}
		t.Fatal(err)
	}
	tracker := NewTracker()
	baseline, err := backend.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tracker.Observe(baseline); err != nil {
		t.Fatal(err)
	}
	transfer := func() (uint64, uint64) {
		t.Helper()
		var incoming, outgoing uint64
		requests := []int{13, 257, 1024}
		responses := []int{7, 31, 97}
		buffer := make([]byte, 2048)
		for i, size := range requests {
			deadline := time.Now().Add(3 * time.Second)
			if err := server.SetDeadline(deadline); err != nil {
				t.Fatal(err)
			}
			if err := client.SetDeadline(deadline); err != nil {
				t.Fatal(err)
			}
			payload := bytes.Repeat([]byte{byte(i + 1)}, size)
			if n, err := client.Write(payload); err != nil || n != len(payload) {
				t.Fatalf("send request: n=%d err=%v", n, err)
			}
			n, peer, err := server.ReadFromUDP(buffer)
			if err != nil || !bytes.Equal(buffer[:n], payload) {
				t.Fatalf("receive request: n=%d err=%v", n, err)
			}
			reply := bytes.Repeat([]byte{byte(i + 10)}, responses[i])
			if n, err := server.WriteToUDP(reply, peer); err != nil || n != len(reply) {
				t.Fatalf("send reply: n=%d err=%v", n, err)
			}
			n, err = client.Read(buffer)
			if err != nil || !bytes.Equal(buffer[:n], reply) {
				t.Fatalf("receive reply: n=%d err=%v", n, err)
			}
			// No fragmentation/options: IPv4 header20 + UDP header8 per datagram.
			incoming += uint64(size + 28)
			outgoing += uint64(len(reply) + 28)
		}
		return incoming, outgoing
	}
	assertCounters := func(generation string, incoming, outgoing uint64, packets uint64) []Counter {
		t.Helper()
		counters, err := backend.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(counters) != 2 {
			t.Fatalf("unexpected counters: %+v", counters)
		}
		for _, counter := range counters {
			want := incoming
			if counter.ScopeID == "udp-out" {
				want = outgoing
			} else if counter.ScopeID != "udp-in" {
				t.Fatalf("unknown scope %q", counter.ScopeID)
			}
			if counter.Generation != generation || counter.Bytes != want || counter.Packets != packets {
				t.Fatalf("kernel accounting mismatch: %+v want bytes=%d packets=%d generation=%s", counter, want, packets, generation)
			}
		}
		return counters
	}
	incoming, outgoing := transfer()
	counted := assertCounters("udp-generation-1", incoming, outgoing, 3)
	deltas, err := tracker.Observe(counted)
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range deltas {
		if delta.Continuity != "complete" || delta.Packets != 3 {
			t.Fatalf("first traffic continuity: %+v", delta)
		}
	}
	if err := backend.Apply(ctx, scopes, "udp-generation-2"); err != nil {
		t.Fatal(err)
	}
	reset := assertCounters("udp-generation-2", 0, 0, 0)
	deltas, err = tracker.Observe(reset)
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range deltas {
		if delta.Reason != "rule-reload" || delta.Continuity != "uncertain" || delta.Bytes != 0 || delta.Packets != 0 {
			t.Fatalf("reset invented usage: %+v", delta)
		}
	}
	incoming, outgoing = transfer()
	counted = assertCounters("udp-generation-2", incoming, outgoing, 3)
	deltas, err = tracker.Observe(counted)
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range deltas {
		if delta.Continuity != "complete" || delta.Packets != 3 {
			t.Fatalf("post-reset continuity: %+v", delta)
		}
	}
	fmt.Printf("udp_kernel_accounting=PASS in_ip_bytes=%d out_ip_bytes=%d packets_each_direction=3 generations=2 topology=local-ipv4-loopback\n", incoming, outgoing)
}
