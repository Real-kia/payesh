package modules

// The modules HTTP adapter exposes the curated catalog and the per-server
// install/enable/disable/remove lifecycle. It never accepts a
// request-provided download URL — Archive/Manifest bytes are verified
// against the pinned trust registry before anything is written to disk.

import (
	"crypto/sha256"
	"encoding/base64"
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

const maxRequestBytes = 4 << 20 // module archives are small but real; bound well above manifest-only requests

var moduleServerIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
var moduleIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

type Service struct {
	Store   *monitoring.Store
	Manager *Manager
}

func NewService(manager *Manager) *Service {
	return &Service{Store: manager.Store, Manager: manager}
}

func (s *Service) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

func (s *Service) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/v1/modules" {
		if r.Method != http.MethodGet {
			writeModulesError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not supported for the module catalog", false)
			return
		}
		writeModulesJSON(w, http.StatusOK, struct {
			Items []contracts.ModuleCatalogEntry `json:"items"`
		}{Catalog})
		return
	}
	const prefix = "/api/v1/servers/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		writeModulesError(w, http.StatusNotFound, "not_found", "resource not found", false)
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, prefix), "/"), "/")
	if len(parts) < 2 || parts[1] != "modules" || !moduleServerIDPattern.MatchString(parts[0]) {
		writeModulesError(w, http.StatusNotFound, "not_found", "resource not found", false)
		return
	}
	serverID := contracts.ServerID(parts[0])
	switch len(parts) {
	case 2:
		if r.Method != http.MethodGet {
			writeModulesError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not supported for module status", false)
			return
		}
		s.status(w, r, serverID)
	case 4:
		if !moduleIDPattern.MatchString(parts[2]) || r.Method != http.MethodPost {
			writeModulesError(w, http.StatusNotFound, "not_found", "resource not found", false)
			return
		}
		s.lifecycle(w, r, serverID, parts[2], parts[3])
	default:
		writeModulesError(w, http.StatusNotFound, "not_found", "resource not found", false)
	}
}

func (s *Service) status(w http.ResponseWriter, r *http.Request, serverID contracts.ServerID) {
	installations, err := s.Store.ListModuleInstallations(r.Context(), serverID)
	if err != nil {
		writeModulesError(w, http.StatusInternalServerError, "storage_error", "could not read module installations", true)
		return
	}
	writeModulesJSON(w, http.StatusOK, struct {
		Items []contracts.ModuleInstallation `json:"items"`
	}{installations})
}

type lifecycleRequest struct {
	IdempotencyKey       string                    `json:"idempotency_key"`
	ExpectedRevision     uint64                    `json:"expected_revision,string"`
	Manifest             *contracts.ModuleManifest `json:"manifest,omitempty"`
	ManifestSignatureB64 string                    `json:"manifest_signature_b64,omitempty"`
	ArchiveBase64        string                    `json:"archive_base64,omitempty"`
}

var moduleActions = map[string]bool{"install": true, "enable": true, "disable": true, "remove": true}

func (s *Service) lifecycle(w http.ResponseWriter, r *http.Request, serverID contracts.ServerID, moduleID, action string) {
	if !moduleActions[action] {
		writeModulesError(w, http.StatusNotFound, "not_found", "resource not found", false)
		return
	}
	var request lifecycleRequest
	if !decodeModulesRequest(w, r, &request) {
		return
	}
	if request.IdempotencyKey == "" || len(request.IdempotencyKey) > 128 {
		writeModulesError(w, http.StatusBadRequest, "invalid_idempotency_key", "idempotency_key must be 1..128 characters", false)
		return
	}
	hash := hashLifecycleRequest(action, request)
	if record, found, err := s.Store.GetModuleLifecycleRequest(r.Context(), serverID, moduleID, request.IdempotencyKey); err != nil {
		writeModulesError(w, http.StatusInternalServerError, "storage_error", "could not read module request history", true)
		return
	} else if found {
		if record.RequestHash != hash {
			writeModulesError(w, http.StatusConflict, "idempotency_conflict", "idempotency_key was already used for a different module request", false)
			return
		}
		var installation contracts.ModuleInstallation
		if err := json.Unmarshal([]byte(record.ResultJSON), &installation); err != nil {
			writeModulesError(w, http.StatusInternalServerError, "storage_error", "stored module result is invalid", true)
			return
		}
		writeModulesJSON(w, http.StatusOK, installation)
		return
	}

	installation, err := s.runAction(r, serverID, moduleID, action, request)
	if err != nil {
		switch {
		case errors.Is(err, monitoring.ErrModuleRevisionConflict):
			writeModulesError(w, http.StatusConflict, "module_revision_conflict", "module state changed; reload before retrying", false)
		case errors.Is(err, ErrModuleExecutorUnavailable):
			writeModulesError(w, http.StatusServiceUnavailable, "module_executor_unavailable", "authenticated module execution is not configured", false)
		default:
			writeModulesError(w, http.StatusBadRequest, "module_lifecycle_failed", err.Error(), false)
		}
		return
	}
	encoded, err := json.Marshal(installation)
	if err != nil {
		writeModulesError(w, http.StatusInternalServerError, "storage_error", "could not encode module result", true)
		return
	}
	if err := s.Store.SaveModuleLifecycleRequest(r.Context(), serverID, moduleID, request.IdempotencyKey, hash, encoded); err != nil {
		writeModulesError(w, http.StatusInternalServerError, "storage_error", "could not persist module request result", true)
		return
	}
	writeModulesJSON(w, http.StatusOK, installation)
}

func (s *Service) runAction(r *http.Request, serverID contracts.ServerID, moduleID, action string, request lifecycleRequest) (contracts.ModuleInstallation, error) {
	switch action {
	case "install":
		if request.Manifest == nil {
			return contracts.ModuleInstallation{}, errors.New("manifest is required to install a module")
		}
		archive, err := base64.StdEncoding.DecodeString(request.ArchiveBase64)
		if err != nil {
			return contracts.ModuleInstallation{}, errors.New("archive_base64 is not valid base64")
		}
		return s.Manager.Install(r.Context(), InstallRequest{
			ServerID: serverID, ModuleID: moduleID, Manifest: *request.Manifest,
			ManifestSignatureB64: request.ManifestSignatureB64, Archive: archive,
			ExpectedRevision: request.ExpectedRevision,
		})
	case "enable":
		return s.Manager.Enable(r.Context(), serverID, moduleID, request.ExpectedRevision)
	case "disable":
		return s.Manager.Disable(r.Context(), serverID, moduleID, request.ExpectedRevision)
	case "remove":
		return s.Manager.Remove(r.Context(), serverID, moduleID, request.ExpectedRevision)
	default:
		return contracts.ModuleInstallation{}, errors.New("unsupported module action")
	}
}

func hashLifecycleRequest(action string, request lifecycleRequest) string {
	encoded, _ := json.Marshal(struct {
		Action string `json:"action"`
		lifecycleRequest
	}{action, request})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func decodeModulesRequest(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		writeModulesError(w, http.StatusBadRequest, "invalid_request", "request body is invalid", false)
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeModulesError(w, http.StatusBadRequest, "invalid_request", "request body must contain one JSON value", false)
		return false
	}
	return true
}

func writeModulesError(w http.ResponseWriter, status int, code, message string, retryable bool) {
	writeModulesJSON(w, status, contracts.Error{Code: code, Message: message, Retryable: retryable})
}

func writeModulesJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil || len(data)+1 > contracts.MaxEnvelopeBytes {
		data = []byte(`{"code":"response_too_large","message":"response exceeds the size limit","retryable":false}`)
		status = http.StatusRequestEntityTooLarge
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}
