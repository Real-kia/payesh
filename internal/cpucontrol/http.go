package cpucontrol

// The CPU Controls HTTP adapter exposes preview (read-only), apply, and
// revert for one explicitly named target. It never discovers or lists
// arbitrary host services itself — the owner names a target, and Apply
// requires (for a process-group target) a process identity the caller
// already resolved, so this adapter cannot become a way to enumerate or
// touch processes it was not told about.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

const maxRequestBytes = 1 << 20

var cpuServerIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

type Service struct {
	Store   *monitoring.Store
	Manager *Manager
}

func NewService(manager *Manager) *Service {
	return &Service{Store: manager.Store, Manager: manager}
}

func (s *Service) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

func (s *Service) serveHTTP(w http.ResponseWriter, r *http.Request) {
	const prefix = "/api/v1/servers/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		writeCPUError(w, http.StatusNotFound, "not_found", "resource not found", false)
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, prefix), "/"), "/")
	if len(parts) < 2 || parts[1] != "cpu-policies" || !cpuServerIDPattern.MatchString(parts[0]) {
		writeCPUError(w, http.StatusNotFound, "not_found", "resource not found", false)
		return
	}
	serverID := contracts.ServerID(parts[0])
	switch len(parts) {
	case 2:
		if r.Method != http.MethodGet {
			writeCPUError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not supported for cpu-policies", false)
			return
		}
		s.list(w, r, serverID)
	case 5:
		targetKind, targetName, action := parts[2], parts[3], parts[4]
		if r.Method != http.MethodPost || (targetKind != TargetKindService && targetKind != TargetKindProcessGroup) {
			writeCPUError(w, http.StatusNotFound, "not_found", "resource not found", false)
			return
		}
		switch action {
		case "preview":
			s.preview(w, r, serverID, targetKind, targetName)
		case "apply":
			s.apply(w, r, serverID, targetKind, targetName)
		case "revert":
			s.revert(w, r, serverID, targetKind, targetName)
		default:
			writeCPUError(w, http.StatusNotFound, "not_found", "resource not found", false)
		}
	default:
		writeCPUError(w, http.StatusNotFound, "not_found", "resource not found", false)
	}
}

func (s *Service) list(w http.ResponseWriter, r *http.Request, serverID contracts.ServerID) {
	policies, err := s.Store.ListControlPolicies(r.Context(), serverID, ModuleID)
	if err != nil {
		writeCPUError(w, http.StatusInternalServerError, "storage_error", "could not read cpu policies", true)
		return
	}
	writeCPUJSON(w, http.StatusOK, struct {
		Items []contracts.ControlPolicy `json:"items"`
	}{policies})
}

type previewRequest struct {
	Millicores uint64 `json:"millicores,string"`
	TotalCores int    `json:"total_cores,omitempty"`
}

func (s *Service) preview(w http.ResponseWriter, r *http.Request, serverID contracts.ServerID, targetKind, targetName string) {
	var request previewRequest
	if !decodeCPURequest(w, r, &request) {
		return
	}
	preview, err := s.Manager.PreviewForServer(r.Context(), serverID, Target{Kind: targetKind, Name: targetName}, request.Millicores, request.TotalCores)
	if err != nil {
		writeCPUError(w, http.StatusBadRequest, "invalid_preview_request", err.Error(), false)
		return
	}
	writeCPUJSON(w, http.StatusOK, preview)
}

type applyRequest struct {
	IdempotencyKey    string `json:"idempotency_key"`
	ExpectedRevision  uint64 `json:"expected_revision,string"`
	Millicores        uint64 `json:"millicores,string"`
	ProcessPID        int    `json:"process_pid,omitempty"`
	ProcessStartTicks uint64 `json:"process_start_ticks,string,omitempty"`
}

func (s *Service) apply(w http.ResponseWriter, r *http.Request, serverID contracts.ServerID, targetKind, targetName string) {
	var request applyRequest
	if !decodeCPURequest(w, r, &request) {
		return
	}
	target := Target{Kind: targetKind, Name: targetName}
	if targetKind == TargetKindProcessGroup {
		if request.ProcessPID <= 0 {
			writeCPUError(w, http.StatusBadRequest, "invalid_request", "process_pid is required for a process-group target", false)
			return
		}
		target.Process = &ProcessIdentity{PID: request.ProcessPID, StartTicks: request.ProcessStartTicks}
	}
	runIdempotent(w, r, s.Store, serverID, targetKind, targetName, request, func() (contracts.ControlPolicy, error) {
		return s.Manager.Apply(r.Context(), serverID, target, request.Millicores, request.ExpectedRevision)
	})
}

type revertRequest struct {
	IdempotencyKey   string `json:"idempotency_key"`
	ExpectedRevision uint64 `json:"expected_revision,string"`
}

func (s *Service) revert(w http.ResponseWriter, r *http.Request, serverID contracts.ServerID, targetKind, targetName string) {
	var request revertRequest
	if !decodeCPURequest(w, r, &request) {
		return
	}
	target := Target{Kind: targetKind, Name: targetName}
	runIdempotent(w, r, s.Store, serverID, targetKind, targetName, request, func() (contracts.ControlPolicy, error) {
		return s.Manager.Revert(r.Context(), serverID, target, request.ExpectedRevision)
	})
}

// idempotencyKeyed is implemented by every mutating request body so
// runIdempotent can read its key without duplicating the check per handler.
type idempotencyKeyed interface {
	idempotencyKeyValue() string
}

func (r applyRequest) idempotencyKeyValue() string  { return r.IdempotencyKey }
func (r revertRequest) idempotencyKeyValue() string { return r.IdempotencyKey }

func runIdempotent(w http.ResponseWriter, r *http.Request, store *monitoring.Store, serverID contracts.ServerID, targetKind, targetName string, request idempotencyKeyed, run func() (contracts.ControlPolicy, error)) {
	key := request.idempotencyKeyValue()
	if key == "" || len(key) > 128 {
		writeCPUError(w, http.StatusBadRequest, "invalid_idempotency_key", "idempotency_key must be 1..128 characters", false)
		return
	}
	encoded, _ := json.Marshal(request)
	sum := sha256.Sum256(encoded)
	hash := hex.EncodeToString(sum[:])

	record, claimed, err := store.ClaimControlPolicyRequest(r.Context(), serverID, ModuleID, targetKind, targetName, key, hash)
	if err != nil {
		if errors.Is(err, monitoring.ErrControlPolicyRequestConflict) {
			writeCPUError(w, http.StatusConflict, "idempotency_conflict", "idempotency_key was already used for a different request", false)
			return
		}
		writeCPUError(w, http.StatusInternalServerError, "storage_error", "could not read cpu policy request history", true)
		return
	} else if !claimed {
		if record.ResultJSON == `{}` {
			writeCPUError(w, http.StatusConflict, "request_in_progress", "an identical request is already running", true)
			return
		}
		var policy contracts.ControlPolicy
		if err := json.Unmarshal([]byte(record.ResultJSON), &policy); err != nil {
			writeCPUError(w, http.StatusInternalServerError, "storage_error", "stored cpu policy result is invalid", true)
			return
		}
		writeCPUJSON(w, http.StatusOK, policy)
		return
	}

	policy, err := run()
	if err != nil {
		_ = store.AbandonControlPolicyRequest(r.Context(), serverID, ModuleID, targetKind, targetName, key, hash)
		switch {
		case errors.Is(err, monitoring.ErrControlPolicyRevisionConflict):
			writeCPUError(w, http.StatusConflict, "control_policy_revision_conflict", "policy state changed; reload before retrying", false)
		default:
			writeCPUError(w, http.StatusBadRequest, "cpu_policy_failed", err.Error(), false)
		}
		return
	}
	resultJSON, err := json.Marshal(policy)
	if err != nil {
		writeCPUError(w, http.StatusInternalServerError, "storage_error", "could not encode cpu policy result", true)
		return
	}
	if err := store.CompleteControlPolicyRequest(r.Context(), serverID, ModuleID, targetKind, targetName, key, hash, resultJSON); err != nil {
		writeCPUError(w, http.StatusInternalServerError, "storage_error", "could not persist cpu policy request result", true)
		return
	}
	writeCPUJSON(w, http.StatusOK, policy)
}

func decodeCPURequest(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		writeCPUError(w, http.StatusBadRequest, "invalid_request", "request body is invalid", false)
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeCPUError(w, http.StatusBadRequest, "invalid_request", "request body must contain one JSON value", false)
		return false
	}
	return true
}

func writeCPUError(w http.ResponseWriter, status int, code, message string, retryable bool) {
	writeCPUJSON(w, status, contracts.Error{Code: code, Message: message, Retryable: retryable})
}

func writeCPUJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil || len(data)+1 > contracts.MaxEnvelopeBytes {
		data = []byte(`{"code":"response_too_large","message":"response exceeds the size limit","retryable":false}`)
		status = http.StatusRequestEntityTooLarge
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}
