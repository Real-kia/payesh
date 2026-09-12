package modules

import "testing"

func TestAuthenticatedUnixModuleProxyRequiresToken(t *testing.T) {
	if _, err := NewAuthenticatedUnixModuleProxy("/tmp/payesh-cpu.sock", ""); err == nil {
		t.Fatal("accepted an empty module proxy token")
	}
	if _, err := NewAuthenticatedUnixModuleProxy("relative.sock", "secret"); err == nil {
		t.Fatal("accepted a relative module socket path")
	}
	if _, err := NewAuthenticatedUnixModuleProxy("/tmp/payesh-cpu.sock", "secret"); err != nil {
		t.Fatalf("valid authenticated proxy rejected: %v", err)
	}
}
