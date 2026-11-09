package fleet

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Real-kia/payesh/internal/alerts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

const destinationsPath = "/api/v1/alerts/destinations"

func isDestinationsPath(path string) bool {
	path = strings.TrimRight(path, "/")
	return path == destinationsPath || strings.HasPrefix(path, destinationsPath+"/")
}

type destinationPage struct {
	Items []alerts.DestinationView `json:"items"`
	Limit int                      `json:"limit"`
}

type previewRequest struct {
	Kind     string `json:"kind,omitempty"`
	Format   string `json:"format"`
	Template string `json:"template,omitempty"`
}

type previewResponse struct {
	Body        string `json:"body"`
	ContentType string `json:"content_type"`
}

// destinationsHandler serves the owner-managed notification destinations
// under /api/v1/alerts/destinations. Reads are open to any authenticated
// account (no secret is ever returned); every change needs the owner.
func (a *API) destinationsHandler(manager *alerts.DestinationManager) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest := strings.Trim(strings.TrimPrefix(strings.TrimRight(r.URL.Path, "/"), destinationsPath), "/")
		parts := []string{}
		if rest != "" {
			parts = strings.Split(rest, "/")
		}
		switch {
		case len(parts) == 0:
			switch r.Method {
			case http.MethodGet:
				a.listDestinations(w, r, manager)
			case http.MethodPost:
				if !a.requireOwner(w, r) {
					return
				}
				var input alerts.DestinationInput
				if !decode(w, r, &input) {
					return
				}
				view, err := manager.Create(r.Context(), input)
				if err != nil {
					writeDestinationError(w, err)
					return
				}
				writeStorageJSON(w, http.StatusCreated, view)
			default:
				methodNotAllowed(w, "GET, POST")
			}
		case len(parts) == 1 && parts[0] == "preview":
			if r.Method != http.MethodPost {
				methodNotAllowed(w, "POST")
				return
			}
			var request previewRequest
			if !decode(w, r, &request) {
				return
			}
			body, contentType, err := alerts.PreviewPayload(request.Kind, request.Format, request.Template)
			if err != nil {
				writeDestinationError(w, err)
				return
			}
			writeStorageJSON(w, http.StatusOK, previewResponse{Body: body, ContentType: contentType})
		case len(parts) == 1:
			a.destinationItem(w, r, manager, parts[0])
		case len(parts) == 2 && parts[1] == "test":
			if r.Method != http.MethodPost {
				methodNotAllowed(w, "POST")
				return
			}
			if !a.requireOwner(w, r) {
				return
			}
			result, err := manager.Test(r.Context(), parts[0])
			if err != nil {
				writeDestinationError(w, err)
				return
			}
			writeStorageJSON(w, http.StatusOK, result)
		default:
			writeFleetError(w, http.StatusNotFound, "not_found", "resource not found", false)
		}
	})
}

func (a *API) listDestinations(w http.ResponseWriter, r *http.Request, manager *alerts.DestinationManager) {
	items, err := manager.List(r.Context())
	if err != nil {
		writeFleetError(w, http.StatusServiceUnavailable, "storage_error", "notification destinations could not be loaded", true)
		return
	}
	writeStorageJSON(w, http.StatusOK, destinationPage{Items: items, Limit: monitoring.MaxAlertDestinations})
}

func (a *API) destinationItem(w http.ResponseWriter, r *http.Request, manager *alerts.DestinationManager, id string) {
	switch r.Method {
	case http.MethodGet:
		view, err := manager.Get(r.Context(), id)
		if err != nil {
			writeDestinationError(w, err)
			return
		}
		writeStorageJSON(w, http.StatusOK, view)
	case http.MethodPatch, http.MethodPut:
		if !a.requireOwner(w, r) {
			return
		}
		var input alerts.DestinationInput
		if !decode(w, r, &input) {
			return
		}
		view, err := manager.Update(r.Context(), id, input)
		if err != nil {
			writeDestinationError(w, err)
			return
		}
		writeStorageJSON(w, http.StatusOK, view)
	case http.MethodDelete:
		if !a.requireOwner(w, r) {
			return
		}
		if err := manager.Delete(r.Context(), id); err != nil {
			writeDestinationError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w, "GET, PATCH, PUT, DELETE")
	}
}

func (a *API) requireOwner(w http.ResponseWriter, r *http.Request) bool {
	principal, ok := a.sessions.Authenticate(r)
	if !ok || principal.Role != "owner" {
		writeFleetError(w, http.StatusForbidden, "owner_required", "only the owner can change notification destinations", false)
		return false
	}
	return true
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeFleetError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
}

func writeDestinationError(w http.ResponseWriter, err error) {
	var invalid alerts.InvalidDestinationError
	switch {
	case errors.As(err, &invalid):
		writeFleetError(w, http.StatusBadRequest, "invalid_destination", invalid.Msg, false)
	case errors.Is(err, monitoring.ErrDestinationNotFound):
		writeFleetError(w, http.StatusNotFound, "not_found", "notification destination not found", false)
	case errors.Is(err, monitoring.ErrDestinationLimit):
		writeFleetError(w, http.StatusConflict, "destination_limit_reached", err.Error(), false)
	case errors.Is(err, alerts.ErrDestinationManaged):
		writeFleetError(w, http.StatusConflict, "managed_by_environment", err.Error(), false)
	case errors.Is(err, alerts.ErrTestThrottled):
		writeFleetError(w, http.StatusTooManyRequests, "test_throttled", err.Error(), true)
	default:
		writeFleetError(w, http.StatusServiceUnavailable, "storage_error", "notification destination could not be saved", true)
	}
}
