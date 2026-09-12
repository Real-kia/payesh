package bandwidth

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/porttraffic"
)

// Runtime groups the optional module's HTTP, enforcement, kernel, and
// recovery components. Constructing it has no kernel side effects.
type Runtime struct {
	Manager  *Manager
	Service  *Service
	Enforcer *Enforcer
	Guard    FileGuard
	Kernel   *TCBackend
}

type RuntimeOptions struct {
	Store                *monitoring.Store
	ServerID             contracts.ServerID
	StateDir             string
	PayeshRemotePorts    []uint16
	PayeshLocalPorts     []uint16
	AdditionalManagement []ManagementFlow
	ManagementRoundTrip  func(context.Context) error
}

func NewRuntime(options RuntimeOptions) (*Runtime, error) {
	if options.Store == nil || options.ServerID == "" || !filepath.IsAbs(options.StateDir) || options.ManagementRoundTrip == nil {
		return nil, errors.New("bandwidth runtime options are incomplete")
	}
	guard := FileGuard{Dir: filepath.Join(options.StateDir, "rollback")}
	kernel := &TCBackend{StateDir: filepath.Join(options.StateDir, "tc")}
	discovery := LocalManagementDiscovery{
		SSHDRunner: porttraffic.ExecRunner{Path: "sshd"}, PayeshRemotePorts: options.PayeshRemotePorts,
		PayeshLocalPorts: options.PayeshLocalPorts, Additional: options.AdditionalManagement,
	}
	manager := &Manager{
		Kernel: kernel, Guard: guard, Store: options.Store, LocalServerID: options.ServerID,
		ManagementRoundTrip: options.ManagementRoundTrip, ManagementDiscovery: discovery.Discover,
	}
	return &Runtime{Manager: manager, Service: NewService(manager), Enforcer: &Enforcer{Store: options.Store, Manager: manager, ServerID: options.ServerID}, Guard: guard, Kernel: kernel}, nil
}
