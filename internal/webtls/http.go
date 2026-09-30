package webtls

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// SettingsRequest is the body of PUT /api/v1/settings/https.
type SettingsRequest struct {
	Domain          string `json:"domain"`
	Email           string `json:"email,omitempty"`
	CloudflareToken string `json:"cloudflare_api_token,omitempty"`
}

// Handler serves GET/PUT/PATCH/DELETE /api/v1/settings/https. Callers must wrap it
// in the browser session and CSRF middleware. ctx bounds background issuance.
func (m *Manager) Handler(ctx context.Context) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeStatus(w, http.StatusOK, m.Status())
		case http.MethodPatch:
			var request struct {
				Port int `json:"port"`
			}
			decoder := json.NewDecoder(io.LimitReader(r.Body, 1024))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&request); err != nil || request.Port < 1 || request.Port > 65535 {
				writeError(w, 400, "invalid_port", "port must be between 1 and 65535")
				return
			}
			if m.portController == nil {
				writeError(w, 503, "port_unavailable", "port changes are unavailable")
				return
			}
			if err := m.portController.Change(request.Port); err != nil {
				writeError(w, 409, "port_unavailable", err.Error())
				return
			}
			writeStatus(w, 200, m.Status())
		case http.MethodPut:
			var request SettingsRequest
			decoder := json.NewDecoder(io.LimitReader(r.Body, 16<<10))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&request); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request", "request body must be a JSON settings object")
				return
			}
			err := m.ConfigureAsync(ctx, Config{Domain: request.Domain, Email: request.Email, CloudflareToken: request.CloudflareToken})
			switch {
			case errors.Is(err, ErrInvalidDomain):
				writeError(w, http.StatusBadRequest, "invalid_domain", err.Error())
			case errors.Is(err, ErrBusy):
				writeError(w, http.StatusConflict, "https_busy", err.Error())
			case err != nil:
				writeError(w, http.StatusInternalServerError, "https_error", err.Error())
			default:
				writeStatus(w, http.StatusAccepted, m.Status())
			}
		case http.MethodDelete:
			if err := m.Disable(); errors.Is(err, ErrBusy) {
				writeError(w, http.StatusConflict, "https_busy", err.Error())
			} else if err != nil {
				writeError(w, http.StatusInternalServerError, "https_error", err.Error())
			} else {
				w.WriteHeader(http.StatusNoContent)
			}
		default:
			w.Header().Set("Allow", "GET, PUT, PATCH, DELETE")
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		}
	})
}

func writeStatus(w http.ResponseWriter, code int, status Status) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(status)
}

func writeError(w http.ResponseWriter, code int, errorCode, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": errorCode, "message": message, "retryable": code >= 500})
}
