package main

import "testing"

func TestAgentTransportConfigRequiresWSSAndRestrictedPaths(t *testing.T) {
	valid := []string{"wss://hub.example.test:9797/node/v1", "/var/lib/payesh/node.json", "/var/lib/payesh/ca.pem", "/var/lib/payesh/spool"}
	config, err := newAgentTransportConfig(valid[0], valid[1], valid[2], valid[3])
	if err != nil || config.URL != valid[0] {
		t.Fatalf("valid agent transport config rejected: config=%+v err=%v", config, err)
	}
	for _, values := range [][4]string{
		{"https://hub.example.test:9797/node/v1", valid[1], valid[2], valid[3]},
		{"ws://hub.example.test:9797/node/v1", valid[1], valid[2], valid[3]},
		{"wss://hub.example.test:9797/node/v1", "relative.json", valid[2], valid[3]},
		{"wss://hub.example.test:9797/node/v1", valid[1], "../ca.pem", valid[3]},
		{"wss://hub.example.test:9797/node/v1", valid[1], valid[2], "spool"},
	} {
		if _, err := newAgentTransportConfig(values[0], values[1], values[2], values[3]); err == nil {
			t.Fatalf("accepted invalid agent transport config: %q", values)
		}
	}
}

func TestNodeTransportURLFromBootstrapUsesNodeRoute(t *testing.T) {
	got, err := nodeTransportURLFromBootstrap("wss://hub.example.test:9797/node/bootstrap/v1?token=must-not-forward")
	if err != nil || got != "wss://hub.example.test:9797/node/v1" {
		t.Fatalf("derived node endpoint=%q err=%v", got, err)
	}
	for _, endpoint := range []string{"", "https://hub.example.test/node/bootstrap/v1", "wss:///node/bootstrap/v1"} {
		if _, err := nodeTransportURLFromBootstrap(endpoint); err == nil {
			t.Fatalf("accepted invalid bootstrap endpoint %q", endpoint)
		}
	}
}
