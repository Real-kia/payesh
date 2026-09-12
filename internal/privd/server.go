package privd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

// ModuleSpec is the helper's curated mapping from an official module to its
// service unit and executable name. Neither field is accepted from a caller.
type ModuleSpec struct {
	UnitTemplate string
	BinaryName   string
}

// Config controls the root-owned helper. Modules must be explicitly listed;
// an unknown module ID is rejected before any filesystem or service action.
type Config struct {
	SocketPath string
	ModuleRoot string
	Modules    map[string]ModuleSpec
	RunCommand func(context.Context, string, ...string) error
}

type Server struct {
	config Config
	mu     sync.Mutex
}

func NewServer(config Config) (*Server, error) {
	if config.SocketPath == "" || !filepath.IsAbs(config.SocketPath) || filepath.Clean(config.SocketPath) != config.SocketPath {
		return nil, errors.New("privd: socket path must be a clean absolute path")
	}
	if config.ModuleRoot == "" || !filepath.IsAbs(config.ModuleRoot) || filepath.Clean(config.ModuleRoot) != config.ModuleRoot {
		return nil, errors.New("privd: module root must be a clean absolute path")
	}
	if len(config.Modules) == 0 {
		return nil, errors.New("privd: no official modules configured")
	}
	for id, spec := range config.Modules {
		if !validIdentifier(id) || spec.UnitTemplate == "" || strings.ContainsAny(spec.UnitTemplate, ";&|`$\n\r") || !validIdentifier(spec.BinaryName) {
			return nil, fmt.Errorf("privd: invalid module specification for %q", id)
		}
	}
	if config.RunCommand == nil {
		config.RunCommand = runSystemctl
	}
	return &Server{config: config}, nil
}

// ListenAndServe creates and owns the configured permission-controlled Unix
// socket. It removes only that exact socket on shutdown.
func (s *Server) ListenAndServe(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("privd: helper must run as root")
	}
	if info, err := os.Lstat(s.config.SocketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("privd: socket path exists and is not a socket")
		}
		if err := os.Remove(s.config.SocketPath); err != nil {
			return fmt.Errorf("privd: remove stale socket: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("privd: inspect socket: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.config.SocketPath), 0o750); err != nil {
		return fmt.Errorf("privd: create socket directory: %w", err)
	}
	listener, err := net.Listen("unix", s.config.SocketPath)
	if err != nil {
		return fmt.Errorf("privd: listen: %w", err)
	}
	defer func() { _ = listener.Close(); _ = os.Remove(s.config.SocketPath) }()
	if err := os.Chmod(s.config.SocketPath, 0o660); err != nil {
		return fmt.Errorf("privd: set socket permissions: %w", err)
	}
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	return s.Serve(listener)
}

func (s *Server) Serve(listener net.Listener) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.handle(conn)
	}
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(maxDeadline))
	var request contracts.ActionRequest
	decoder := json.NewDecoder(io.LimitReader(conn, maxRequestBytes))
	if err := decoder.Decode(&request); err != nil {
		s.writeResponse(conn, contracts.ActionResponse{Accepted: false, Error: helperError("invalid_request", "request is invalid")})
		return
	}
	response := contracts.ActionResponse{RequestID: request.RequestID}
	if err := s.validateRequest(request); err != nil {
		response.Error = helperError("request_rejected", err.Error())
		s.writeResponse(conn, response)
		return
	}
	var invocation ModuleInvocation
	if err := json.Unmarshal(request.Arguments, &invocation); err != nil {
		response.Error = helperError("invalid_arguments", "module arguments are invalid")
		s.writeResponse(conn, response)
		return
	}
	if err := invocation.Validate(s.config.ModuleRoot); err != nil || invocation.ModuleID != request.Target || invocation.ServerID != request.TargetServerID {
		response.Error = helperError("invalid_arguments", "module arguments do not match the request")
		s.writeResponse(conn, response)
		return
	}
	// Serialize privileged transitions. A stop racing a start can otherwise
	// leave systemd and the durable module state describing different worlds.
	s.mu.Lock()
	err := s.invoke(context.Background(), invocation)
	s.mu.Unlock()
	if err != nil {
		response.Error = helperError("module_invoke_failed", err.Error())
		s.writeResponse(conn, response)
		return
	}
	response.Accepted = true
	s.writeResponse(conn, response)
}

func (s *Server) validateRequest(request contracts.ActionRequest) error {
	if request.Protocol != contracts.HelperProtocol || request.Action != "module.invoke" {
		return errors.New("only module.invoke is supported")
	}
	if request.RequestID == "" || len(request.RequestID) > 128 || request.IdempotencyKey == "" || len(request.IdempotencyKey) > 128 {
		return errors.New("request identity is invalid")
	}
	if !validIdentifier(request.Target) || !validServerID(request.TargetServerID) {
		return errors.New("request target is invalid")
	}
	if request.Deadline.IsZero() || !request.Deadline.After(time.Now()) || request.Deadline.After(time.Now().Add(maxDeadline+time.Second)) {
		return errors.New("request deadline is invalid")
	}
	if len(request.Arguments) == 0 || len(request.Arguments) > maxRequestBytes {
		return errors.New("request arguments exceed the size limit")
	}
	if _, ok := s.config.Modules[request.Target]; !ok {
		return errors.New("module is not in the helper allowlist")
	}
	return nil
}

func (s *Server) invoke(ctx context.Context, invocation ModuleInvocation) error {
	spec := s.config.Modules[invocation.ModuleID]
	installDir := filepath.Clean(invocation.InstallDir)
	binaryPath := filepath.Join(installDir, "bin", spec.BinaryName)
	info, err := os.Stat(binaryPath)
	if err != nil {
		return fmt.Errorf("module executable is unavailable")
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return errors.New("module executable is not runnable")
	}
	switch invocation.Operation {
	case "health":
		return nil
	case "enable":
		return s.config.RunCommand(ctx, "enable", "--now", formatUnit(spec.UnitTemplate, invocation.ServerID))
	case "disable":
		return s.config.RunCommand(ctx, "disable", "--now", formatUnit(spec.UnitTemplate, invocation.ServerID))
	default:
		return errors.New("module operation is not allowed")
	}
}

func formatUnit(template string, serverID contracts.ServerID) string {
	return strings.ReplaceAll(template, "%s", string(serverID))
}

func runSystemctl(ctx context.Context, action string, args ...string) error {
	path := "/usr/bin/systemctl"
	if _, err := os.Stat(path); err != nil {
		path = "/bin/systemctl"
	}
	commandArgs := append([]string{action}, args...)
	command := exec.CommandContext(ctx, path, commandArgs...)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return errors.New("system service action failed")
	}
	return nil
}

func helperError(code, message string) *contracts.Error {
	return &contracts.Error{Code: code, Message: message, Retryable: code == "module_invoke_failed"}
}

func (s *Server) writeResponse(conn net.Conn, response contracts.ActionResponse) {
	_ = json.NewEncoder(conn).Encode(response)
}
