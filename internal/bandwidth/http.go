package bandwidth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/porttraffic"
)

const maxHTTPBody = 1 << 20

var bandwidthServerIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

type Service struct {
	Store   *monitoring.Store
	Manager *Manager
	mu      sync.Mutex
}

func NewService(manager *Manager) *Service { return &Service{Store: manager.Store, Manager: manager} }
func (s *Service) Handler() http.Handler   { return http.HandlerFunc(s.serveHTTP) }

func (s *Service) serveHTTP(w http.ResponseWriter, r *http.Request) {
	const prefix = "/api/v1/servers/"
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, prefix), "/"), "/")
	if !strings.HasPrefix(r.URL.Path, prefix) || len(parts) < 2 || parts[1] != "bandwidth-policies" || !bandwidthServerIDPattern.MatchString(parts[0]) {
		writeBandwidthError(w, http.StatusNotFound, "not_found", "resource not found", false)
		return
	}
	serverID := contracts.ServerID(parts[0])
	if len(parts) == 2 && r.Method == http.MethodGet {
		policies, err := s.Store.ListControlPolicies(r.Context(), serverID, ModuleID)
		if err != nil {
			writeBandwidthError(w, http.StatusInternalServerError, "storage_error", "could not read bandwidth policies", true)
			return
		}
		writeBandwidthJSON(w, http.StatusOK, struct {
			Items []contracts.ControlPolicy `json:"items"`
		}{policies})
		return
	}
	if len(parts) != 5 || r.Method != http.MethodPost || (parts[2] != "interface" && parts[2] != "local-port") {
		writeBandwidthError(w, http.StatusNotFound, "not_found", "resource not found", false)
		return
	}
	targetKind, targetName := parts[2], parts[3]
	switch parts[4] {
	case "preview":
		s.preview(w, r, serverID, targetKind, targetName)
	case "apply":
		s.apply(w, r, serverID, targetKind, targetName)
	case "revert":
		s.revert(w, r, serverID, targetKind, targetName)
	default:
		writeBandwidthError(w, http.StatusNotFound, "not_found", "resource not found", false)
	}
}

type policyRequest struct {
	IdempotencyKey   string                `json:"idempotency_key,omitempty"`
	ExpectedRevision uint64                `json:"expected_revision,string,omitempty"`
	Interface        string                `json:"interface"`
	Protocol         porttraffic.Protocol  `json:"protocol,omitempty"`
	LocalPort        uint16                `json:"local_port,omitempty"`
	Direction        porttraffic.Direction `json:"direction"`
	Action           Action                `json:"action"`
	BitsPerSecond    uint64                `json:"bits_per_second,string,omitempty"`
	QuotaBytes       uint64                `json:"quota_bytes,string,omitempty"`
	UsageScope       string                `json:"usage_scope,omitempty"`
	UsageDirection   string                `json:"usage_direction,omitempty"`
}

func (p policyRequest) model(targetKind string) Request {
	return Request{Scope: Scope{TargetKind: targetKind, Interface: p.Interface, Protocol: p.Protocol, LocalPort: p.LocalPort, Direction: p.Direction}, Action: p.Action, BitsPerSecond: p.BitsPerSecond, QuotaBytes: p.QuotaBytes, UsageScope: p.UsageScope, UsageDirection: p.UsageDirection}
}

func (s *Service) preview(w http.ResponseWriter, r *http.Request, serverID contracts.ServerID, targetKind, targetName string) {
	var request policyRequest
	if !decodeBandwidth(w, r, &request) {
		return
	}
	model := request.model(targetKind)
	if model.Scope.TargetName() != targetName {
		writeBandwidthError(w, http.StatusBadRequest, "target_mismatch", "URL target does not match request scope", false)
		return
	}
	preview, err := s.Manager.PreviewPolicy(r.Context(), serverID, model)
	if err != nil {
		writeBandwidthError(w, http.StatusBadRequest, "invalid_policy", err.Error(), false)
		return
	}
	writeBandwidthJSON(w, http.StatusOK, preview)
}

func (s *Service) apply(w http.ResponseWriter, r *http.Request, serverID contracts.ServerID, targetKind, targetName string) {
	var request policyRequest
	if !decodeBandwidth(w, r, &request) {
		return
	}
	model := request.model(targetKind)
	if model.Scope.TargetName() != targetName {
		writeBandwidthError(w, http.StatusBadRequest, "target_mismatch", "URL target does not match request scope", false)
		return
	}
	s.runIdempotent(w, r, serverID, model.Scope, request, func() (contracts.ControlPolicy, error) {
		digest := sha256.Sum256([]byte(string(serverID) + "\x00" + targetName + "\x00" + request.IdempotencyKey))
		return s.Manager.ApplyPolicy(r.Context(), serverID, "bw-"+hex.EncodeToString(digest[:12]), model, request.ExpectedRevision)
	})
}

func (s *Service) revert(w http.ResponseWriter, r *http.Request, serverID contracts.ServerID, targetKind, targetName string) {
	var request policyRequest
	if !decodeBandwidth(w, r, &request) {
		return
	}
	scope := request.model(targetKind).Scope
	if scope.TargetName() != targetName {
		writeBandwidthError(w, http.StatusBadRequest, "target_mismatch", "URL target does not match request scope", false)
		return
	}
	s.runIdempotent(w, r, serverID, scope, request, func() (contracts.ControlPolicy, error) {
		return s.Manager.RevertPolicy(r.Context(), serverID, scope, request.ExpectedRevision)
	})
}

func (s *Service) runIdempotent(w http.ResponseWriter, r *http.Request, serverID contracts.ServerID, scope Scope, request policyRequest, run func() (contracts.ControlPolicy, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if request.IdempotencyKey == "" || len(request.IdempotencyKey) > 128 {
		writeBandwidthError(w, http.StatusBadRequest, "invalid_idempotency_key", "idempotency_key must be 1..128 characters", false)
		return
	}
	encoded, _ := json.Marshal(request)
	digest := sha256.Sum256(encoded)
	hash := hex.EncodeToString(digest[:])
	record, claimed, err := s.Store.ClaimControlPolicyRequest(r.Context(), serverID, ModuleID, scope.TargetKind, scope.TargetName(), request.IdempotencyKey, hash)
	if err != nil {
		if errors.Is(err, monitoring.ErrControlPolicyRequestConflict) {
			writeBandwidthError(w, http.StatusConflict, "idempotency_conflict", "idempotency key was used for different input", false)
			return
		}
		writeBandwidthError(w, http.StatusInternalServerError, "storage_error", "could not read request history", true)
		return
	}
	if !claimed {
		if record.ResultJSON == `{}` {
			writeBandwidthError(w, http.StatusConflict, "request_in_progress", "an identical request is already running", true)
			return
		}
		var policy contracts.ControlPolicy
		if json.Unmarshal([]byte(record.ResultJSON), &policy) != nil {
			writeBandwidthError(w, http.StatusInternalServerError, "storage_error", "stored policy result is invalid", true)
			return
		}
		writeBandwidthJSON(w, http.StatusOK, policy)
		return
	}
	policy, err := run()
	if err != nil {
		_ = s.Store.AbandonControlPolicyRequest(r.Context(), serverID, ModuleID, scope.TargetKind, scope.TargetName(), request.IdempotencyKey, hash)
		status, code := http.StatusBadRequest, "bandwidth_policy_failed"
		if errors.Is(err, monitoring.ErrControlPolicyRevisionConflict) || errors.Is(err, ErrForeignNetworkState) {
			status, code = http.StatusConflict, "control_policy_conflict"
		}
		writeBandwidthError(w, status, code, err.Error(), false)
		return
	}
	result, _ := json.Marshal(policy)
	if err := s.Store.CompleteControlPolicyRequest(r.Context(), serverID, ModuleID, scope.TargetKind, scope.TargetName(), request.IdempotencyKey, hash, result); err != nil {
		writeBandwidthError(w, http.StatusInternalServerError, "storage_error", "could not persist request result", true)
		return
	}
	writeBandwidthJSON(w, http.StatusOK, policy)
}

func decodeBandwidth(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxHTTPBody)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil {
		writeBandwidthError(w, http.StatusBadRequest, "invalid_request", "request body is invalid", false)
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeBandwidthError(w, http.StatusBadRequest, "invalid_request", "request body must contain one JSON value", false)
		return false
	}
	return true
}

func writeBandwidthError(w http.ResponseWriter, status int, code, message string, retryable bool) {
	writeBandwidthJSON(w, status, contracts.Error{Code: code, Message: message, Retryable: retryable})
}
func writeBandwidthJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil || len(data)+1 > contracts.MaxEnvelopeBytes {
		data, status = []byte(`{"code":"response_too_large","message":"response exceeds limit","retryable":false}`), http.StatusRequestEntityTooLarge
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}
