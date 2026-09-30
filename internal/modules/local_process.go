package modules

// ProcessRuntime supervises the read-only optional process module as the same
// unprivileged user as payesh-server. It never grants capabilities or accepts
// an executable/command from the browser.
import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

const ProcessModuleID = "process-monitoring"

type ProcessRuntime struct {
	Context  context.Context
	Store    *monitoring.Store
	Root     string
	ServerID contracts.ServerID
	Fallback ModuleExecutor
	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
}

func (p *ProcessRuntime) Socket() string {
	digest := sha256.Sum256([]byte(p.ServerID))
	return filepath.Join(p.Root, fmt.Sprintf("process-%x.sock", digest[:12]))
}

func (p *ProcessRuntime) Invoke(ctx context.Context, in ModuleInvocation) error {
	if in.ModuleID != ProcessModuleID {
		if p.Fallback != nil {
			return p.Fallback.Invoke(ctx, in)
		}
		return ErrModuleExecutorUnavailable
	}
	if !moduleServerIDPattern.MatchString(string(p.ServerID)) || in.ServerID != p.ServerID {
		return fmt.Errorf("%w: process package must target the local server", ErrModuleExecutorUnavailable)
	}
	if !filepath.IsAbs(p.Root) || filepath.Clean(p.Root) != p.Root {
		return errors.New("invalid module root")
	}
	expected := filepath.Join(p.Root, string(p.ServerID), ProcessModuleID)
	// Health is also used for the verified staging directory before activation.
	if filepath.Dir(in.InstallDir) != expected {
		return errors.New("process executable is outside its installation")
	}
	binary := filepath.Join(in.InstallDir, "bin", ProcessModuleID)
	switch in.Operation {
	case "health":
		command := exec.CommandContext(ctx, binary, "--check")
		command.Stdout = io.Discard
		command.Stderr = io.Discard
		if e := command.Run(); e != nil {
			return errors.New("process module cannot read Linux proc counters")
		}
		return nil
	case "disable":
		return p.stop(ctx)
	case "enable":
		if filepath.Base(in.InstallDir) != "active" {
			return errors.New("only the active process release can be enabled")
		}
		return p.start(ctx, binary)
	default:
		return errors.New("unsupported process module operation")
	}
}

func (p *ProcessRuntime) stop(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel == nil {
		return nil
	}
	p.cancel()
	select {
	case <-p.done:
		p.cancel = nil
		p.done = nil
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *ProcessRuntime) start(ctx context.Context, binary string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		select {
		case <-p.done:
			p.cancel = nil
		default:
			return nil
		}
	}
	parent := p.Context
	if parent == nil {
		return errors.New("process supervisor context is required")
	}
	child, cancel := context.WithCancel(parent)
	start := func() (*exec.Cmd, error) {
		cmd := exec.CommandContext(child, binary, "--server-id", string(p.ServerID), "--socket", p.Socket())
		cmd.Stdout = io.Discard
		cmd.Stderr = os.Stderr
		return cmd, cmd.Start()
	}
	cmd, e := start()
	if e != nil {
		cancel()
		return fmt.Errorf("start process module: %w", e)
	}
	done := make(chan struct{})
	p.cancel = cancel
	p.done = done
	go func() {
		command := cmd
		defer close(done)
		for {
			_ = command.Wait()
			if child.Err() != nil {
				return
			}
			timer := time.NewTimer(5 * time.Second)
			select {
			case <-child.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			for {
				next, startErr := start()
				if startErr == nil {
					command = next
					break
				}
				timer := time.NewTimer(5 * time.Second)
				select {
				case <-child.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}
	}()
	proxy, e := NewUnixModuleProxy(p.Socket())
	if e != nil {
		cancel()
		<-done
		p.cancel = nil
		return e
	}
	probeCtx, probeCancel := context.WithTimeout(ctx, 8*time.Second)
	defer probeCancel()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		request, _ := http.NewRequestWithContext(probeCtx, http.MethodGet, "http://module.local/api/v1/servers/"+string(p.ServerID)+"/processes?limit=1", nil)
		capture := &healthResponse{header: http.Header{}}
		proxy.ServeHTTP(capture, request)
		if capture.status == http.StatusOK {
			return nil
		}
		select {
		case <-ctx.Done():
			cancel()
			<-done
			p.cancel = nil
			return ctx.Err()
		case <-deadline.C:
			cancel()
			<-done
			p.cancel = nil
			return errors.New("process module did not produce a healthy sample")
		case <-ticker.C:
		}
	}
}

type healthResponse struct {
	header http.Header
	status int
}

func (w *healthResponse) Header() http.Header    { return w.header }
func (w *healthResponse) WriteHeader(status int) { w.status = status }
func (w *healthResponse) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return len(b), nil
}

func (p *ProcessRuntime) Restore(ctx context.Context) error {
	if p.ServerID == "" {
		return nil
	}
	state, e := p.Store.GetModuleInstallation(ctx, p.ServerID, ProcessModuleID)
	if e != nil {
		return e
	}
	if state.State != contracts.ModuleEnabled {
		return nil
	}
	return p.Invoke(ctx, ModuleInvocation{ServerID: p.ServerID, ModuleID: ProcessModuleID, ModuleVersion: state.Version, Operation: "enable", InstallDir: filepath.Join(p.Root, string(p.ServerID), ProcessModuleID, "active")})
}

func (p *ProcessRuntime) Handler() http.Handler {
	proxy, e := NewUnixModuleProxy(p.Socket())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/servers/"+string(p.ServerID)+"/processes" {
			writeModulesError(w, 404, "unsupported_target", "process monitoring is available on the local Payesh server", false)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeModulesError(w, 405, "method_not_allowed", "process monitoring is read-only", false)
			return
		}
		state, err := p.Store.GetModuleInstallation(r.Context(), p.ServerID, ProcessModuleID)
		if err != nil {
			writeModulesError(w, 503, "storage_error", "could not read process package state", true)
			return
		}
		if state.State != contracts.ModuleEnabled {
			writeModulesError(w, 409, "package_disabled", "enable Advanced Process Monitoring in Packages", false)
			return
		}
		if e != nil {
			writeModulesError(w, 503, "module_executor_unavailable", "process monitoring is unavailable", true)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}
