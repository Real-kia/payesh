package cpucontrol

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ServiceManager identifies the supervisor from which a discovered service
// came. Discovery is intentionally read-only; applying a policy still needs
// the privileged supervisor adapter configured on Manager.
type ServiceManager string

const (
	ServiceManagerSystemd ServiceManager = "systemd"
	ServiceManagerOpenRC  ServiceManager = "openrc"
)

// DiscoveredService is a selectable service target. Active state is advisory
// and may change immediately after discovery; it must never be used as proof
// that a policy was applied.
type DiscoveredService struct {
	Target      Target         `json:"target"`
	Manager     ServiceManager `json:"manager"`
	LoadState   string         `json:"load_state,omitempty"`
	ActiveState string         `json:"active_state,omitempty"`
	SubState    string         `json:"sub_state,omitempty"`
	Description string         `json:"description,omitempty"`
}

// ErrServiceDiscoveryUnsupported means that neither a usable systemd nor an
// OpenRC discovery command is available. This is an expected capability
// result on hosts using another init system, not an internal failure.
var ErrServiceDiscoveryUnsupported = errors.New("cpucontrol: service discovery is unsupported on this host")

// CommandRunner is injectable to keep parsing and fallback behavior testable
// without requiring systemd or OpenRC on the development machine.
type CommandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ServiceDiscovery lists service targets without mutating supervisor state.
// A zero Runner uses exec.CommandContext. The first successful backend wins;
// a failed systemd query falls back to OpenRC, which allows this code to run
// on minimal containers and non-systemd Linux installations.
type ServiceDiscovery struct {
	Runner CommandRunner
}

func (d ServiceDiscovery) runner() CommandRunner {
	if d.Runner != nil {
		return d.Runner
	}
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).Output()
	}
}

// List returns service targets in deterministic name order. An empty result
// is valid when a supervisor exists but currently has no selectable services.
func (d ServiceDiscovery) List(ctx context.Context) ([]DiscoveredService, error) {
	run := d.runner()
	if output, err := run(ctx, "systemctl", "list-units", "--type=service", "--all", "--no-legend", "--no-pager", "--plain"); err == nil {
		services := parseSystemdServices(string(output))
		if services != nil {
			return services, nil
		}
	}
	if output, err := run(ctx, "rc-status", "--all", "--servicelist"); err == nil {
		services := parseOpenRCServices(string(output))
		if services != nil {
			return services, nil
		}
	}
	if output, err := run(ctx, "rc-update", "show"); err == nil {
		services := parseOpenRCServices(string(output))
		if services != nil {
			return services, nil
		}
	}
	return nil, ErrServiceDiscoveryUnsupported
}

func parseSystemdServices(output string) []DiscoveredService {
	seen := make(map[string]struct{})
	services := make([]DiscoveredService, 0)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "0 loaded") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 || !strings.HasSuffix(fields[0], ".service") {
			continue
		}
		name := fields[0]
		if !isDiscoverableServiceName(name) {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		services = append(services, DiscoveredService{
			Target: Target{Kind: TargetKindService, Name: name}, Manager: ServiceManagerSystemd,
			LoadState: fields[1], ActiveState: fields[2], SubState: fields[3],
			Description: strings.TrimSpace(strings.TrimPrefix(line, strings.Join(fields[:4], " "))),
		})
	}
	return sortDiscoveredServices(services)
}

func parseOpenRCServices(output string) []DiscoveredService {
	seen := make(map[string]struct{})
	services := make([]DiscoveredService, 0)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Runlevel:") || strings.HasPrefix(line, "Dynamic:") || strings.HasPrefix(line, "service") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		// rc-status --servicelist emits one service per line. rc-update show
		// prefixes services with a runlevel, so accept the final field too.
		name := fields[0]
		for index, field := range fields {
			if field == "|" {
				name = ""
				for _, candidate := range fields[index+1:] {
					if isDiscoverableServiceName(candidate) {
						name = candidate
						break
					}
				}
				break
			}
		}
		if !isDiscoverableServiceName(name) {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		services = append(services, DiscoveredService{Target: Target{Kind: TargetKindService, Name: name}, Manager: ServiceManagerOpenRC, ActiveState: "configured"})
	}
	return sortDiscoveredServices(services)
}

func isDiscoverableServiceName(name string) bool {
	if name == "" {
		return false
	}
	return Target{Kind: TargetKindService, Name: name}.validateKindName() == nil
}

func sortDiscoveredServices(services []DiscoveredService) []DiscoveredService {
	// Insertion sort keeps this helper allocation-free beyond the result slice
	// and the expected service list is small. Stable ordering makes API output
	// and UI pagination deterministic across hosts.
	for i := 1; i < len(services); i++ {
		for j := i; j > 0 && services[j].Target.Name < services[j-1].Target.Name; j-- {
			services[j], services[j-1] = services[j-1], services[j]
		}
	}
	return services
}

// DiscoverServices is the default convenience entry point used by local
// callers. Pass a bounded context because supervisor commands are external.
func DiscoverServices(ctx context.Context) ([]DiscoveredService, error) {
	if ctx == nil {
		return nil, fmt.Errorf("cpucontrol: discovery context is nil")
	}
	return (ServiceDiscovery{}).List(ctx)
}
