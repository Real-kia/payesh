package collector

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewEpochIsFreshWhenNoOverrideIsProvided(t *testing.T) {
	first := NewEpoch()
	second := NewEpoch()
	if first == "" || second == "" || first == second {
		t.Fatalf("collector epochs were not fresh: %q %q", first, second)
	}
	firstCollector := NewCollector("/does-not-exist", DefaultServerID, "")
	secondCollector := NewCollector("/does-not-exist", DefaultServerID, "")
	if firstCollector.Epoch == DefaultEpoch || firstCollector.Epoch == secondCollector.Epoch {
		t.Fatalf("empty epoch override reused a fixed value: %q %q", firstCollector.Epoch, secondCollector.Epoch)
	}
}

func TestPersistentServerIDSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity", "server-id")
	first, err := LoadOrCreateServerID(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateServerID(path)
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || first != second || first == DefaultServerID {
		t.Fatalf("persistent identity did not survive restart: %q %q", first, second)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0077 != 0 {
		t.Fatalf("identity file is too permissive: %o", info.Mode().Perm())
	}
}

func TestCollectorIsSQLiteFreeAndEmitsNodeShape(t *testing.T) {
	metricCollector := NewCollector("/does-not-exist", DefaultServerID, DefaultEpoch)
	sample, err := metricCollector.Collect(context.Background(), time.Unix(100, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := sample.Validate(); err != nil {
		t.Fatal(err)
	}
	if sample.Sequence != 0 || sample.ObservedAt.IsZero() {
		t.Fatalf("unexpected collector sample: %#v", sample)
	}
	if _, err := sample.WithReceivedAt(time.Unix(101, 0).UTC()); err != nil {
		t.Fatal(err)
	}
}

func TestDiskIOOverflowRemainsUnavailable(t *testing.T) {
	counters := map[string]string{}
	units := map[string]string{}
	validity := map[string]string{}
	collectDiskIO([]byte("8 0 sda 1 0 18446744073709551615 1 1 0 1 1 0 0 0\n"), counters, units, validity)
	if validity["disk.io.read_bytes"] != "unavailable" || len(counters) != 0 {
		t.Fatalf("disk overflow was accepted as a partial total: counters=%#v validity=%#v", counters, validity)
	}
}

func TestNetworkBillingScansBeyondInterfaceDisplayCap(t *testing.T) {
	var data strings.Builder
	for index := 0; index < maxNetworkInterfaces; index++ {
		fmt.Fprintf(&data, "veth%d: 1 2 3 4 5 6 7 8 9 10 11 12\n", index)
	}
	data.WriteString("eth0: 123 2 3 4 5 6 7 8 9 10 11 12\n")
	counters := map[string]string{}
	units := map[string]string{}
	validity := map[string]string{}
	collectNetwork([]byte(data.String()), counters, units, validity)
	if counters["net.billing.rx_bytes"] != "123" || validity["net.billing.rx_bytes"] != "valid" {
		t.Fatalf("billing total ignored an interface after the display cap: counters=%#v validity=%#v", counters, validity)
	}
	if _, displayed := counters["net.eth0.rx_bytes"]; displayed {
		t.Fatal("interface beyond the display cap was unexpectedly emitted")
	}
}

func TestNetworkBillingExcludesLogicalInterfaces(t *testing.T) {
	data := "br0: 100 0 0 0 0 0 0 0 100 0 0 0\n" +
		"bond0: 200 0 0 0 0 0 0 0 200 0 0 0\n" +
		"eth0.100: 300 0 0 0 0 0 0 0 300 0 0 0\n" +
		"eth0: 7 0 0 0 0 0 0 0 11 0 0 0\n"
	counters, units, validity := map[string]string{}, map[string]string{}, map[string]string{}
	collectNetwork([]byte(data), counters, units, validity)
	if counters["net.billing.rx_bytes"] != "7" || counters["net.billing.tx_bytes"] != "11" {
		t.Fatalf("logical interfaces inflated billing baseline: %#v", counters)
	}
}

func TestNetworkBillingHonorsAuthoritativeInterfaceSelection(t *testing.T) {
	data := "eth0: 7 0 0 0 0 0 0 0 11 0 0 0\n" +
		"eth1: 100 0 0 0 0 0 0 0 200 0 0 0\n"
	counters, units, validity := map[string]string{}, map[string]string{}, map[string]string{}
	collectNetworkWithRootAndSelection("", []byte(data), counters, units, validity, []string{"eth1"})
	if counters["net.billing.rx_bytes"] != "100" || counters["net.billing.tx_bytes"] != "200" || validity["net.billing.rx_bytes"] != "valid" {
		t.Fatalf("authoritative interface selection was ignored: %#v %#v", counters, validity)
	}
}

func TestSelectedInterfaceBeyondDisplayCapIsEmitted(t *testing.T) {
	var data strings.Builder
	for index := 0; index < maxNetworkInterfaces; index++ {
		fmt.Fprintf(&data, "veth%d: 1 2 3 4 5 6 7 8 9 10 11 12\n", index)
	}
	data.WriteString("wan0: 321 2 3 4 5 6 7 8 654 10 11 12\n")
	counters := map[string]string{}
	units := map[string]string{}
	validity := map[string]string{}
	collectNetworkWithRootAndSelection("", []byte(data.String()), counters, units, validity, []string{"wan0"})
	if counters["net.wan0.rx_bytes"] != "321" || counters["net.wan0.tx_bytes"] != "654" || validity["net.wan0.rx_bytes"] != "valid" {
		t.Fatalf("explicitly selected interface past display cap was omitted: counters=%#v validity=%#v", counters, validity)
	}
}

func TestMissingLoadAndMemoryFieldsAreExplicitlyUnavailable(t *testing.T) {
	values, units, validity := map[string]float64{}, map[string]string{}, map[string]string{}
	collectLoad([]byte("0.1"), values, units, validity)
	for _, metric := range []string{"load.1m", "load.5m", "load.15m"} {
		if validity[metric] == "" {
			t.Fatalf("missing load field was not marked unavailable: %s %#v", metric, validity)
		}
	}
	counters := map[string]string{}
	collectMemory([]byte("MemTotal: 1024 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n"), counters, values, units, validity)
	for _, metric := range []string{"memory.available_bytes", "memory.used_bytes", "memory.used_percent"} {
		if validity[metric] != "unavailable" {
			t.Fatalf("dependent memory field was not marked unavailable: %s %#v", metric, validity)
		}
	}
}
