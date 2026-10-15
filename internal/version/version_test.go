package version

import "testing"

func TestNodeVersionFallsBackToRelease(t *testing.T) {
	oldValue, oldNode := Value, Node
	defer func() { Value, Node = oldValue, oldNode }()
	Value, Node = "1.2.3", ""
	if NodeVersion() != "1.2.3" {
		t.Fatalf("unstamped node version=%q", NodeVersion())
	}
	Node = "1.1.0"
	if NodeVersion() != "1.1.0" {
		t.Fatalf("stamped node version=%q", NodeVersion())
	}
}
