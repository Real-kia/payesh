// Package privd implements the local, typed boundary between Payesh and the
// root-owned privileged helper. It deliberately has no shell-string or
// arbitrary-file API.
package privd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

const (
	DefaultSocketPath = "/run/payesh/privd.sock"
	maxRequestBytes   = 64 << 10
	maxDeadline       = 30 * time.Second
)

// ModuleInvocation is the only operation exposed by the package-06 helper
// implementation. InstallDir is checked against the helper's configured
// module root before it can be used.
type ModuleInvocation struct {
	ServerID      contracts.ServerID `json:"server_id"`
	ModuleID      string             `json:"module_id"`
	ModuleVersion string             `json:"module_version"`
	Operation     string             `json:"operation"`
	InstallDir    string             `json:"install_dir"`
}

func (r ModuleInvocation) Validate(root string) error {
	if !validServerID(r.ServerID) || !validIdentifier(r.ModuleID) || r.ModuleVersion == "" || len(r.ModuleVersion) > 64 {
		return errors.New("privd: module identity is invalid")
	}
	switch r.Operation {
	case "health", "enable", "disable":
	default:
		return errors.New("privd: module operation is not allowed")
	}
	if !filepath.IsAbs(r.InstallDir) || filepath.Clean(r.InstallDir) != r.InstallDir {
		return errors.New("privd: install directory must be a clean absolute path")
	}
	cleanRoot := filepath.Clean(root)
	if !filepath.IsAbs(cleanRoot) {
		return errors.New("privd: module root must be absolute")
	}
	rel, err := filepath.Rel(cleanRoot, r.InstallDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("privd: install directory is outside the module root")
	}
	return nil
}

// Client sends one bounded, newline-delimited typed request to payesh-privd.
// Socket permissions are the local caller boundary; the helper remains the
// authority for action and path validation.
type Client struct {
	SocketPath string
	Timeout    time.Duration
}

func (c Client) Invoke(ctx context.Context, invocation ModuleInvocation) error {
	if c.SocketPath == "" || !filepath.IsAbs(c.SocketPath) || filepath.Clean(c.SocketPath) != c.SocketPath {
		return errors.New("privd: socket path must be a clean absolute path")
	}
	if err := invocation.Validate(filepath.Dir(filepath.Dir(c.SocketPath))); err != nil {
		// The client cannot know the configured module root. The helper repeats
		// this validation; retain identity/operation validation locally while
		// allowing a deployment-specific module root.
		if strings.Contains(err.Error(), "outside the module root") || strings.Contains(err.Error(), "module root") {
			// continue; the server performs the authoritative root check
		} else {
			return err
		}
	}
	requestID, err := newRequestID()
	if err != nil {
		return err
	}
	deadline := time.Now().UTC().Add(maxDeadline)
	if requested, ok := ctx.Deadline(); ok && requested.Before(deadline) {
		deadline = requested
	}
	args, err := json.Marshal(invocation)
	if err != nil {
		return err
	}
	request := contracts.ActionRequest{
		Protocol: contracts.HelperProtocol, RequestID: requestID,
		Action: "module.invoke", Target: invocation.ModuleID,
		TargetServerID: invocation.ServerID, IdempotencyKey: requestID,
		Deadline: deadline, Arguments: args,
	}
	response := c.Execute(ctx, request)
	if response.Accepted {
		return nil
	}
	if response.Error == nil {
		return errors.New("privd: helper rejected request")
	}
	return fmt.Errorf("privd: %s", response.Error.Message)
}

// Execute forwards an already authenticated hub action to the local typed
// helper without changing its request or idempotency identity. The helper is
// still authoritative for the action allowlist and target validation.
func (c Client) Execute(ctx context.Context, request contracts.ActionRequest) contracts.ActionResponse {
	failure := func(code, message string, retryable bool) contracts.ActionResponse {
		return contracts.ActionResponse{RequestID: request.RequestID, Error: &contracts.Error{Code: code, Message: message, Retryable: retryable}}
	}
	if c.SocketPath == "" || !filepath.IsAbs(c.SocketPath) || filepath.Clean(c.SocketPath) != c.SocketPath {
		return failure("privd_not_configured", "privileged helper socket is not configured", false)
	}
	if err := request.Validate(time.Now().UTC()); err != nil {
		return failure("invalid_action", err.Error(), false)
	}
	timeout := c.Timeout
	if timeout <= 0 || timeout > maxDeadline {
		timeout = maxDeadline
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(dialCtx, "unix", c.SocketPath)
	if err != nil {
		return failure("privd_unavailable", "privileged helper is unavailable", true)
	}
	defer conn.Close()
	deadline := time.Now().UTC().Add(timeout)
	if request.Deadline.Before(deadline) {
		deadline = request.Deadline
	}
	_ = conn.SetDeadline(deadline)
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return failure("privd_send_failed", "could not send action to privileged helper", true)
	}
	var response contracts.ActionResponse
	decoder := json.NewDecoder(io.LimitReader(conn, maxRequestBytes))
	if err := decoder.Decode(&response); err != nil {
		return failure("privd_response_invalid", "privileged helper returned an invalid response", true)
	}
	if response.RequestID != request.RequestID {
		return failure("privd_response_mismatch", "privileged helper response identity mismatch", false)
	}
	return response
}

func newRequestID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("privd: generate request id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func validIdentifier(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r == '-' || r == '_' || r == '.' || r == ':' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func validServerID(value contracts.ServerID) bool {
	if len(value) < 16 || len(value) > 128 {
		return false
	}
	return validIdentifier(string(value))
}
