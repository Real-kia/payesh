package cpucontrol

// Runtime contains the process-level wiring for the optional CPU Controls
// module.  The policy manager remains deliberately independent from process
// startup and Unix sockets so it can continue to be tested with a fixture
// filesystem.  Runtime is the production boundary: it opens the real cgroup
// v2 mount, prepares the Payesh hierarchy, and serves the authenticated local
// HTTP API.

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

const (
	DefaultCgroupRoot   = "/sys/fs/cgroup"
	DefaultCPUStateRoot = "/var/lib/payesh/cpu-controls"
	DefaultCPUSocket    = "/run/payesh/cpu-controls.sock"
)

// ServicePreparer is the supervisor-aware portion of service targeting.  It
// must prove that the named service is already in the supplied Payesh-owned
// group; implementations must not move an unrelated/shared service cgroup.
type ServicePreparer interface {
	Prepare(context.Context, Target, GroupPath) error
}

// SupervisorRunner is injectable for tests and for distributions whose init
// tooling lives outside the usual PATH.
type SupervisorRunner func(context.Context, string, ...string) ([]byte, error)

// LocalServicePreparer supports the conservative subset that can be proven
// safe without rewriting a supervisor's unit configuration.  A service must
// report the exact Payesh-owned cgroup selected by Target.GroupPath.  This
// avoids accidentally applying a policy to system.slice (or an OpenRC shared
// group), and leaves unsupported arrangements visible to the operator.
type LocalServicePreparer struct {
	FS     CgroupFS
	Runner SupervisorRunner
}

func (p LocalServicePreparer) Prepare(ctx context.Context, target Target, group GroupPath) error {
	if target.Kind != TargetKindService {
		return errors.New("cpucontrol: service preparer received a non-service target")
	}
	if p.FS == nil {
		return errors.New("cpucontrol: service preparer has no cgroup filesystem")
	}
	if p.Runner == nil {
		return errors.New("cpucontrol: service supervisor is unavailable")
	}
	owned, err := p.FS.IsDedicatedGroup(group)
	if err != nil {
		return err
	}
	if !owned {
		return errors.New("cpucontrol: service cgroup is not Payesh-owned")
	}
	unit := normalizeServiceUnit(target.Name)
	manager, err := p.detectManager(ctx)
	if err != nil {
		return err
	}
	var controlGroup string
	switch manager {
	case "systemd":
		active, err := p.Runner(ctx, "systemctl", "show", unit, "--property=ActiveState", "--value", "--no-pager")
		if err != nil {
			return fmt.Errorf("cpucontrol: inspect systemd service: %w", err)
		}
		if strings.TrimSpace(string(active)) != "active" {
			return fmt.Errorf("cpucontrol: service %s is not active", unit)
		}
		output, err := p.Runner(ctx, "systemctl", "show", unit, "--property=ControlGroup", "--value", "--no-pager")
		if err != nil {
			return fmt.Errorf("cpucontrol: inspect systemd cgroup: %w", err)
		}
		controlGroup = strings.TrimSpace(string(output))
	case "openrc":
		// OpenRC does not provide one portable command for a service's cgroup.
		// Refuse shared/unknown arrangements rather than guessing from a PID.
		return errors.New("cpucontrol: OpenRC service cgroup ownership cannot be proven on this host")
	default:
		return errors.New("cpucontrol: unsupported service supervisor")
	}
	controlGroup = strings.TrimPrefix(filepath.Clean(controlGroup), string(filepath.Separator))
	if controlGroup != string(group) {
		return fmt.Errorf("cpucontrol: service %s runs in cgroup %q, expected Payesh-owned %q", unit, controlGroup, group)
	}
	empty, err := p.FS.IsEmpty(group)
	if err != nil {
		return err
	}
	if empty {
		return fmt.Errorf("cpucontrol: service %s has no processes in its dedicated cgroup", unit)
	}
	return nil
}

func (p LocalServicePreparer) detectManager(ctx context.Context) (string, error) {
	if _, err := p.Runner(ctx, "systemctl", "--version"); err == nil {
		return "systemd", nil
	}
	if _, err := p.Runner(ctx, "rc-status", "--version"); err == nil {
		return "openrc", nil
	}
	return "", errors.New("cpucontrol: no supported service supervisor was found")
}

func normalizeServiceUnit(name string) string {
	if strings.HasSuffix(name, ".service") {
		return name
	}
	return name + ".service"
}

// Runtime is safe to construct without touching the kernel.  Call Serve (or
// Prepare) to perform the privileged cgroup hierarchy setup.
type Runtime struct {
	Manager    *Manager
	Service    *Service
	Cgroup     FSCgroup
	Preparer   ServicePreparer
	AuthToken  string
	SocketPath string

	prepareOnce sync.Once
	prepareErr  error
}

type RuntimeOptions struct {
	Store         *monitoring.Store
	ServerID      contracts.ServerID
	CgroupRoot    string
	OwnershipRoot string
	ProcRoot      string
	SocketPath    string
	AuthToken     string
	Runner        SupervisorRunner
	Preparer      ServicePreparer
}

func NewRuntime(options RuntimeOptions) (*Runtime, error) {
	if options.Store == nil {
		return nil, errors.New("cpucontrol: runtime store is required")
	}
	if options.ServerID == "" {
		return nil, errors.New("cpucontrol: runtime server identity is required")
	}
	root := options.CgroupRoot
	if root == "" {
		root = DefaultCgroupRoot
	}
	ownership := options.OwnershipRoot
	if ownership == "" {
		ownership = filepath.Join(DefaultCPUStateRoot, "ownership")
	}
	socket := options.SocketPath
	if socket == "" {
		socket = DefaultCPUSocket
	}
	for label, value := range map[string]string{"cgroup root": root, "ownership root": ownership, "socket": socket} {
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return nil, fmt.Errorf("cpucontrol: %s must be a clean absolute path", label)
		}
	}
	fs := FSCgroup{Root: root, OwnershipRoot: ownership}
	manager := &Manager{Store: options.Store, FS: fs, ProcRoot: options.ProcRoot, LocalServerID: options.ServerID}
	preparer := options.Preparer
	if preparer == nil {
		preparer = LocalServicePreparer{FS: fs, Runner: options.Runner}
	}
	manager.PrepareService = preparer.Prepare
	runtime := &Runtime{Manager: manager, Service: NewService(manager), Cgroup: fs, Preparer: preparer, AuthToken: options.AuthToken, SocketPath: socket}
	return runtime, nil
}

// Prepare establishes the Payesh cgroup hierarchy exactly once.  A failed
// preparation is retained so callers cannot continue after a partial setup.
func (r *Runtime) Prepare() error {
	r.prepareOnce.Do(func() { r.prepareErr = r.Cgroup.EnsureCPUHierarchy() })
	return r.prepareErr
}

// Handler applies an optional shared-secret check in addition to Unix socket
// permissions.  The default deployment relies on the socket's root:payesh
// ownership boundary; setting AuthToken adds an explicit application-level
// identity check for reverse proxies or unusual service managers.
func (r *Runtime) Handler() http.Handler {
	handler := r.Service.Handler()
	if r.AuthToken == "" {
		return handler
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		provided := req.Header.Get("X-Payesh-Module-Token")
		if len(provided) != len(r.AuthToken) || subtle.ConstantTimeCompare([]byte(provided), []byte(r.AuthToken)) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"code":"module_authentication_required","message":"module authentication failed","retryable":false}`+"\n")
			return
		}
		handler.ServeHTTP(w, req)
	})
}

// ListenAndServe owns only the configured socket path and removes it on
// shutdown.  Unix filesystem permissions are set before accepting requests.
func (r *Runtime) ListenAndServe(ctx context.Context) error {
	if err := r.Prepare(); err != nil {
		return err
	}
	listener, err := listenCPUSocket(r.SocketPath)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close(); _ = os.Remove(r.SocketPath) }()
	server := &http.Server{Handler: r.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func listenCPUSocket(path string) (net.Listener, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("cpucontrol: socket path must be clean and absolute")
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("cpucontrol: socket path exists and is not a socket")
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("cpucontrol: remove stale socket: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return listener, nil
}
