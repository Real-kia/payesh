package cpucontrol

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestParseSystemdServicesSortsAndSkipsMalformedRows(t *testing.T) {
	services := parseSystemdServices(`  zeta.service loaded active running Zeta worker
alpha.service loaded inactive dead Alpha worker
not-a-service loaded active running ignored
alpha.service loaded active running duplicate
0 loaded units listed.`)
	if got := []string{services[0].Target.Name, services[1].Target.Name}; !reflect.DeepEqual(got, []string{"alpha.service", "zeta.service"}) {
		t.Fatalf("unexpected service order: %#v", got)
	}
	if services[0].ActiveState != "inactive" || services[0].SubState != "dead" || services[0].Manager != ServiceManagerSystemd {
		t.Fatalf("unexpected parsed service: %#v", services[0])
	}
}

func TestServiceDiscoveryFallsBackToOpenRC(t *testing.T) {
	called := make([]string, 0)
	discovery := ServiceDiscovery{Runner: func(_ context.Context, name string, _ ...string) ([]byte, error) {
		called = append(called, name)
		if name == "systemctl" {
			return nil, errors.New("not systemd")
		}
		if name == "rc-status" {
			return []byte("acpid\ncron\n"), nil
		}
		return nil, errors.New("unexpected fallback")
	}}
	services, err := discovery.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{services[0].Target.Name, services[1].Target.Name}; !reflect.DeepEqual(got, []string{"acpid", "cron"}) {
		t.Fatalf("unexpected OpenRC targets: %#v", got)
	}
	if services[0].Manager != ServiceManagerOpenRC || len(called) != 2 {
		t.Fatalf("unexpected fallback state: services=%#v calls=%v", services, called)
	}
}

func TestServiceDiscoveryOpenRCUpdateShow(t *testing.T) {
	discovery := ServiceDiscovery{Runner: func(_ context.Context, name string, _ ...string) ([]byte, error) {
		if name == "systemctl" || name == "rc-status" {
			return nil, errors.New("unavailable")
		}
		return []byte("boot | acpid cron\ndefault | sshd"), nil
	}}
	services, err := discovery.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{services[0].Target.Name, services[1].Target.Name}; !reflect.DeepEqual(got, []string{"acpid", "sshd"}) {
		t.Fatalf("unexpected rc-update targets: %#v", got)
	}
}

func TestServiceDiscoveryReportsUnsupported(t *testing.T) {
	discovery := ServiceDiscovery{Runner: func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("missing")
	}}
	_, err := discovery.List(context.Background())
	if !errors.Is(err, ErrServiceDiscoveryUnsupported) {
		t.Fatalf("expected unsupported error, got %v", err)
	}
}

func TestDiscoverServicesRejectsNilContext(t *testing.T) {
	if _, err := DiscoverServices(nil); err == nil {
		t.Fatal("expected nil context to be rejected")
	}
}
