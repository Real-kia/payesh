package monitoring

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

func TestCollectorPreservesUnavailableAndDetectsResets(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proc", "net"), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, "proc", name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("stat", "cpu 100 0 100 800 0 0 0 0\n")
	write("meminfo", "MemTotal:       1024 kB\nMemAvailable:    512 kB\nSwapTotal:       0 kB\nSwapFree:        0 kB\n")
	write("loadavg", "0.25 0.50 0.75 1/100 123\n")
	write("uptime", "42.5 100.0\n")
	write("net/dev", "Inter-| Receive | Transmit\n eth0: 100 2 3 4 0 0 0 0 200 3 4 5 0 0 0 0\n veth0: 999 1 0 0 0 0 0 0 999 1 0 0 0 0 0 0\n")
	write("diskstats", "8 0 sda 10 0 20 30 40 0 50 60 0 0 0\n8 1 sda1 100 0 200 300 400 0 500 600 0 0 0\n")
	collector := NewCollector(root, "server-local-0001", "epoch-local-0001")
	first, err := collector.Collect(context.Background(), time.Unix(100, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if first.Validity["cpu.utilization"] != "unavailable" {
		t.Fatalf("first CPU sample should be unavailable: %#v", first.Validity)
	}
	if first.Counters["net.billing.rx_bytes"] != "100" || first.Counters["net.billing.tx_bytes"] != "200" {
		t.Fatalf("virtual interface leaked into billing counters: %#v", first.Counters)
	}
	if first.Units["net.eth0.rx_bytes"] != "bytes" || first.Units["net.eth0.rx_packets"] != "count" {
		t.Fatalf("network counter units are ambiguous: %#v", first.Units)
	}
	if first.Counters["memory.available_bytes"] != "524288" {
		t.Fatalf("memory unit conversion failed: %#v", first.Counters)
	}
	if first.Counters["disk.io.read_bytes"] != "10240" || first.Counters["disk.io.write_bytes"] != "25600" {
		t.Fatalf("disk I/O counters were not aggregated from base devices: %#v", first.Counters)
	}
	write("stat", "cpu 150 0 150 900 0 0 0 0\n")
	second, err := collector.Collect(context.Background(), time.Unix(115, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if second.Values["cpu.utilization"] != 50 {
		t.Fatalf("unexpected CPU utilization: %#v", second.Values)
	}
	write("stat", "cpu 10 0 10 20 0 0 0 0\n")
	third, err := collector.Collect(context.Background(), time.Unix(130, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if third.Validity["cpu.utilization"] != "uncertain" {
		t.Fatalf("counter reset should be uncertain: %#v", third.Validity)
	}
	if _, ok := third.Values["cpu.utilization"]; ok {
		t.Fatal("counter reset manufactured a utilization value")
	}
}

func TestCollectorDetectsPartialCPUComponentReset(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proc", "net"), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, "proc", name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("stat", "cpu 100 0 100 800 0 0 0 0\n")
	write("meminfo", "MemTotal: 1024 kB\nMemAvailable: 512 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n")
	write("loadavg", "0.1 0.1 0.1 1/1 1\n")
	write("uptime", "1 1\n")
	write("net/dev", "Inter-| Receive | Transmit\n eth0: 1 0 0 0 0 0 0 0 1 0 0 0 0 0 0 0\n")
	write("diskstats", "8 0 sda 1 0 1 1 1 0 1 1 0 0 0\n")
	collector := NewCollector(root, "server-local-0001", "epoch-local-0001")
	if _, err := collector.Collect(context.Background(), time.Unix(100, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	// User decreases while system/idle increase enough to keep the aggregate
	// total increasing. Aggregate-only checks would incorrectly mark this valid.
	write("stat", "cpu 90 0 220 1000 0 0 0 0\n")
	second, err := collector.Collect(context.Background(), time.Unix(115, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if second.Validity["cpu.utilization"] != "uncertain" {
		t.Fatalf("partial CPU reset was treated as valid: %#v", second)
	}
	if _, ok := second.Values["cpu.utilization"]; ok {
		t.Fatal("partial CPU reset manufactured utilization")
	}
}

func TestCollectorProducesValidNodeWireShape(t *testing.T) {
	collector := NewCollector("/does-not-exist", "server-local-0001", "epoch-local-0001")
	sample, err := collector.Collect(context.Background(), time.Unix(100, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := sample.Validate(); err != nil {
		t.Fatal(err)
	}
	if sample.ObservedAt.IsZero() || sample.Sequence != 0 {
		t.Fatalf("unexpected node sample identity: %#v", sample)
	}
	if _, err := sample.WithReceivedAt(time.Unix(101, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	if _, ok := any(sample).(contracts.NodeMetricSample); !ok {
		t.Fatal("sample type changed")
	}
}
