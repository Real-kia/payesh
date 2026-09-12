package porttraffic

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

const maxHTTPBody = 1 << 20

var serverIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

type Service struct {
	Store   *monitoring.Store
	Manager *Manager
}

func NewService(manager *Manager) *Service {
	if manager == nil {
		return &Service{}
	}
	return &Service{Store: manager.Store, Manager: manager}
}

func (s *Service) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

func (s *Service) serveHTTP(w http.ResponseWriter, r *http.Request) {
	const prefix = "/api/v1/servers/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		writePortTrafficError(w, http.StatusNotFound, "not_found", "resource not found", false)
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, prefix), "/"), "/")
	if len(parts) < 2 || parts[1] != "port-traffic-scopes" || !serverIDPattern.MatchString(parts[0]) {
		writePortTrafficError(w, http.StatusNotFound, "not_found", "resource not found", false)
		return
	}
	serverID := contracts.ServerID(parts[0])
	if len(parts) == 2 && r.Method == http.MethodGet {
		s.list(w, r, serverID)
		return
	}
	if len(parts) == 2 && r.Method == http.MethodPost {
		s.configure(w, r, serverID)
		return
	}
	if len(parts) == 4 && r.Method == http.MethodPost && parts[3] == "revert" && safeScopeID(parts[2]) {
		s.revert(w, r, serverID, parts[2])
		return
	}
	writePortTrafficError(w, http.StatusNotFound, "not_found", "resource not found", false)
}

func (s *Service) list(w http.ResponseWriter, r *http.Request, serverID contracts.ServerID) {
	if s.Manager == nil {
		writePortTrafficError(w, http.StatusServiceUnavailable, "unavailable", "port traffic manager is not configured", true)
		return
	}
	policies, err := s.Manager.List(r.Context(), serverID)
	if err != nil {
		writePortTrafficError(w, http.StatusInternalServerError, "storage_error", "could not read port traffic scopes", true)
		return
	}
	writePortTrafficJSON(w, http.StatusOK, struct {
		Items []contracts.ControlPolicy `json:"items"`
	}{policies})
}

type scopeRequest struct {
	IdempotencyKey   string `json:"idempotency_key"`
	ExpectedRevision uint64 `json:"expected_revision,string"`
	Scope            Scope  `json:"scope"`
}

func (s *Service) configure(w http.ResponseWriter, r *http.Request, serverID contracts.ServerID) {
	var request scopeRequest
	if !decodePortTraffic(w, r, &request) {
		return
	}
	if request.IdempotencyKey == "" || len(request.IdempotencyKey) > 128 {
		writePortTrafficError(w, http.StatusBadRequest, "invalid_idempotency_key", "idempotency_key must be 1..128 characters", false)
		return
	}
	if err := request.Scope.Validate(); err != nil {
		writePortTrafficError(w, http.StatusBadRequest, "invalid_scope", err.Error(), false)
		return
	}
	hash := hashScopeRequest("configure", request)
	record, claimed, err := s.Store.ClaimControlPolicyRequest(r.Context(), serverID, ModuleID, "local-port", request.Scope.ID, request.IdempotencyKey, hash)
	if err != nil {
		if errors.Is(err, monitoring.ErrControlPolicyRequestConflict) {
			writePortTrafficError(w, http.StatusConflict, "idempotency_conflict", "idempotency key was used for different input", false)
			return
		}
		writePortTrafficError(w, http.StatusInternalServerError, "storage_error", "could not read request history", true)
		return
	}
	if !claimed {
		if record.ResultJSON == `{}` {
			writePortTrafficError(w, http.StatusConflict, "request_in_progress", "an identical request is already running", true)
			return
		}
		var policy contracts.ControlPolicy
		if json.Unmarshal([]byte(record.ResultJSON), &policy) != nil {
			writePortTrafficError(w, http.StatusInternalServerError, "storage_error", "stored scope result is invalid", true)
			return
		}
		writePortTrafficJSON(w, http.StatusOK, policy)
		return
	}
	if s.Manager == nil {
		writePortTrafficError(w, http.StatusServiceUnavailable, "unavailable", "port traffic manager is not configured", true)
		return
	}
	policy, err := s.Manager.Configure(r.Context(), serverID, request.Scope, request.ExpectedRevision)
	if err != nil {
		_ = s.Store.AbandonControlPolicyRequest(r.Context(), serverID, ModuleID, "local-port", request.Scope.ID, request.IdempotencyKey, hash)
		status, code := http.StatusBadRequest, "invalid_scope"
		if errors.Is(err, monitoring.ErrControlPolicyRevisionConflict) {
			status, code = http.StatusConflict, "control_policy_conflict"
		}
		writePortTrafficError(w, status, code, err.Error(), false)
		return
	}
	result, _ := json.Marshal(policy)
	if err := s.Store.CompleteControlPolicyRequest(r.Context(), serverID, ModuleID, "local-port", request.Scope.ID, request.IdempotencyKey, hash, result); err != nil {
		writePortTrafficError(w, http.StatusInternalServerError, "storage_error", "could not persist request result", true)
		return
	}
	writePortTrafficJSON(w, http.StatusOK, policy)
}

func (s *Service) revert(w http.ResponseWriter, r *http.Request, serverID contracts.ServerID, scopeID string) {
	var request struct {
		IdempotencyKey   string `json:"idempotency_key"`
		ExpectedRevision uint64 `json:"expected_revision,string"`
	}
	if !decodePortTraffic(w, r, &request) {
		return
	}
	if request.IdempotencyKey == "" || len(request.IdempotencyKey) > 128 {
		writePortTrafficError(w, http.StatusBadRequest, "invalid_idempotency_key", "idempotency_key must be 1..128 characters", false)
		return
	}
	hash := hashScopeRequest("revert", request)
	record, claimed, err := s.Store.ClaimControlPolicyRequest(r.Context(), serverID, ModuleID, "local-port", scopeID, request.IdempotencyKey, hash)
	if err != nil {
		if errors.Is(err, monitoring.ErrControlPolicyRequestConflict) {
			writePortTrafficError(w, http.StatusConflict, "idempotency_conflict", "idempotency key was used for different input", false)
			return
		}
		writePortTrafficError(w, http.StatusInternalServerError, "storage_error", "could not read request history", true)
		return
	}
	if !claimed {
		if record.ResultJSON == `{}` {
			writePortTrafficError(w, http.StatusConflict, "request_in_progress", "an identical request is already running", true)
			return
		}
		var policy contracts.ControlPolicy
		if json.Unmarshal([]byte(record.ResultJSON), &policy) != nil {
			writePortTrafficError(w, http.StatusInternalServerError, "storage_error", "stored scope result is invalid", true)
			return
		}
		writePortTrafficJSON(w, http.StatusOK, policy)
		return
	}
	if s.Manager == nil {
		writePortTrafficError(w, http.StatusServiceUnavailable, "unavailable", "port traffic manager is not configured", true)
		return
	}
	policy, err := s.Manager.Revert(r.Context(), serverID, scopeID, request.ExpectedRevision)
	if err != nil {
		_ = s.Store.AbandonControlPolicyRequest(r.Context(), serverID, ModuleID, "local-port", scopeID, request.IdempotencyKey, hash)
		status, code := http.StatusBadRequest, "port_traffic_revert_failed"
		if errors.Is(err, monitoring.ErrControlPolicyRevisionConflict) {
			status, code = http.StatusConflict, "control_policy_conflict"
		}
		writePortTrafficError(w, status, code, err.Error(), false)
		return
	}
	result, _ := json.Marshal(policy)
	if err := s.Store.CompleteControlPolicyRequest(r.Context(), serverID, ModuleID, "local-port", scopeID, request.IdempotencyKey, hash, result); err != nil {
		writePortTrafficError(w, http.StatusInternalServerError, "storage_error", "could not persist request result", true)
		return
	}
	writePortTrafficJSON(w, http.StatusOK, policy)
}

func hashScopeRequest(action string, request any) string {
	encoded, _ := json.Marshal(struct {
		Action  string `json:"action"`
		Request any    `json:"request"`
	}{action, request})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func decodePortTraffic(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxHTTPBody)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil {
		writePortTrafficError(w, http.StatusBadRequest, "invalid_request", "request body is invalid", false)
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writePortTrafficError(w, http.StatusBadRequest, "invalid_request", "request body must contain one JSON value", false)
		return false
	}
	return true
}

func writePortTrafficError(w http.ResponseWriter, status int, code, message string, retryable bool) {
	writePortTrafficJSON(w, status, contracts.Error{Code: code, Message: message, Retryable: retryable})
}

func writePortTrafficJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil || len(data)+1 > contracts.MaxEnvelopeBytes {
		data, status = []byte(`{"code":"response_too_large","message":"response exceeds the size limit","retryable":false}`), http.StatusRequestEntityTooLarge
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}
