package bandwidth

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Real-kia/payesh/internal/porttraffic"
)

// LocalManagementDiscovery derives SSH ports from sshd's effective
// configuration and combines them with installed Payesh transport endpoints.
// These are trusted local inputs, never browser-provided exclusions.
type LocalManagementDiscovery struct {
	SSHDRunner        porttraffic.CommandRunner
	PayeshRemotePorts []uint16
	PayeshLocalPorts  []uint16
	Additional        []ManagementFlow
	AllowNoManagement bool
}

func (d LocalManagementDiscovery) Discover(ctx context.Context, scope Scope) ([]ManagementFlow, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if d.SSHDRunner == nil {
		return nil, errors.New("sshd effective-configuration probe is not configured")
	}
	result, err := d.SSHDRunner.Run(ctx, []string{"-T"}, nil)
	if err != nil {
		message := strings.TrimSpace(string(result.Stderr))
		if len(message) > 256 {
			message = message[:256]
		}
		return nil, fmt.Errorf("read effective sshd configuration: %s: %w", message, err)
	}
	flows := make([]ManagementFlow, 0, 8)
	seen := make(map[string]struct{})
	add := func(flow ManagementFlow) error {
		key := fmt.Sprintf("%s:%s:%s:%d", flow.Name, flow.Protocol, flow.PortRole, flow.LocalPort)
		if _, exists := seen[key]; exists {
			return nil
		}
		seen[key] = struct{}{}
		flow.Interface = scope.Interface
		if !safeName.MatchString(flow.Name) || flow.LocalPort == 0 || (flow.PortRole != "local-service" && flow.PortRole != "remote-service") {
			return errors.New("invalid trusted management flow")
		}
		flows = append(flows, flow)
		return nil
	}
	for _, line := range strings.Split(string(result.Stdout), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "port" {
			continue
		}
		port, parseErr := strconv.ParseUint(fields[1], 10, 16)
		if parseErr != nil || port == 0 {
			return nil, errors.New("sshd returned an invalid port")
		}
		if err := add(ManagementFlow{Name: "ssh", Protocol: porttraffic.TCP, LocalPort: uint16(port), PortRole: "local-service"}); err != nil {
			return nil, err
		}
	}
	for _, port := range d.PayeshRemotePorts {
		if err := add(ManagementFlow{Name: "payesh-hub", Protocol: porttraffic.TCP, LocalPort: port, PortRole: "remote-service"}); err != nil {
			return nil, err
		}
	}
	for _, port := range d.PayeshLocalPorts {
		if err := add(ManagementFlow{Name: "payesh-web", Protocol: porttraffic.TCP, LocalPort: port, PortRole: "local-service"}); err != nil {
			return nil, err
		}
	}
	for _, flow := range d.Additional {
		if err := add(flow); err != nil {
			return nil, err
		}
	}
	if len(flows) == 0 && !d.AllowNoManagement {
		return nil, errors.New("no management flow could be established safely")
	}
	return flows, nil
}
