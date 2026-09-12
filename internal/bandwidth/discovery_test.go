package bandwidth

import (
	"context"
	"testing"

	"github.com/Real-kia/payesh/internal/porttraffic"
)

func TestLocalManagementDiscoveryUsesEffectiveCustomSSHAndPayeshPorts(t *testing.T) {
	runner := &tcFakeRunner{results: []porttraffic.CommandResult{{Stdout: []byte("port 2222\nport 2200\n")}}, errors: []error{nil}}
	discovery := LocalManagementDiscovery{SSHDRunner: runner, PayeshRemotePorts: []uint16{443}, PayeshLocalPorts: []uint16{8787}}
	flows, err := discovery.Discover(context.Background(), Scope{TargetKind: "interface", Interface: "eth0", Direction: porttraffic.Outbound})
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 4 {
		t.Fatalf("flows=%+v", flows)
	}
	if flows[0].LocalPort != 2222 || flows[0].PortRole != "local-service" || flows[2].PortRole != "remote-service" {
		t.Fatalf("flows=%+v", flows)
	}
	if managementPortKey(flows[0], porttraffic.Outbound) != "src_port" || managementPortKey(flows[2], porttraffic.Outbound) != "dst_port" {
		t.Fatalf("wrong management selectors: %+v", flows)
	}
}
