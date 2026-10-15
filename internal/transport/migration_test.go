package transport

import (
	"path/filepath"
	"testing"
)

func TestAgentEndpointPersistenceAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	a := &AgentClient{URL: "wss://hub.example.test/node/v1", IdentityPath: path}
	if err := a.LoadEndpoint(); err != nil {
		t.Fatal(err)
	}
	if err := a.SaveEndpoint("wss://hub.example.test:9797/node/v1", a.URL); err != nil {
		t.Fatal(err)
	}
	b := &AgentClient{URL: a.URL, IdentityPath: path}
	if err := b.LoadEndpoint(); err != nil {
		t.Fatal(err)
	}
	if b.URL != "wss://hub.example.test:9797/node/v1" || b.previousURL != a.URL {
		t.Fatalf("endpoint=%+v", b)
	}
	for _, target := range []string{"ws://hub.example.test:9797/node/v1", "wss://other.example.test:9797/node/v1", "wss://hub.example.test:9797/node/v1?token=x", "wss://hub.example.test:99999/node/v1", "wss://user@hub.example.test:9797/node/v1"} {
		if validMigration(a.URL, target) {
			t.Fatalf("unsafe target %s accepted", target)
		}
	}
	c := &AgentClient{URL: "wss://replacement.example.test:9797/node/v1", IdentityPath: path}
	if err := c.LoadEndpoint(); err != nil || c.URL != "wss://replacement.example.test:9797/node/v1" {
		t.Fatal("explicit configuration was overridden")
	}
}
