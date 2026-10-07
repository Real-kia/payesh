package install

import "testing"

func TestValidateListenAddressRejectsShellMetacharacters(t *testing.T) {
	for _, address := range []string{
		"$(touch /etc/pwned):8787",
		"`id`:8787",
		"host;reboot:8787",
		"host&x:8787",
		"host|x:8787",
		"(host):8787",
		"host<x>:8787",
		"host*:8787",
		"host{a}:8787",
		"host~:8787",
		"host#x:8787",
		"host!:8787",
		"host$HOME:8787",
		"host%00:8787",
		"host:8787;x",
	} {
		if err := validateListenAddress(address); err == nil {
			t.Errorf("validateListenAddress(%q) accepted an address with shell metacharacters", address)
		}
	}
}

func TestValidateListenAddressAcceptsOrdinaryAddresses(t *testing.T) {
	for _, address := range []string{
		"127.0.0.1:8787",
		"0.0.0.0:8080",
		"127.0.0.1:0",
		"[::1]:8787",
		"[2001:db8::1]:443",
		"localhost:8787",
		"payesh-hub.example.com:8787",
		"my_host:8787",
	} {
		if err := validateListenAddress(address); err != nil {
			t.Errorf("validateListenAddress(%q) = %v, want nil", address, err)
		}
	}
}
