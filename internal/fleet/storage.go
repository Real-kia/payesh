package fleet

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Real-kia/payesh/internal/monitoring"
)

func (a *API) storageSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		result, err := a.store.StorageStatus(r.Context())
		if err != nil {
			writeFleetError(w, 503, "storage_unavailable", "storage settings could not be loaded", true)
			return
		}
		writeStorageJSON(w, 200, result)
	case http.MethodPut:
		cookie, err := r.Cookie(a.sessions.SessionCookieName())
		if err != nil {
			writeFleetError(w, 403, "owner_required", "only the owner can change storage settings", false)
			return
		}
		account, ok := a.sessions.SessionAccount(cookie.Value)
		if !ok || account.Role != "owner" {
			writeFleetError(w, 403, "owner_required", "only the owner can change storage settings", false)
			return
		}
		var request monitoring.StorageSettings
		if !decode(w, r, &request) {
			return
		}
		if err := request.Validate(); err != nil {
			writeFleetError(w, 400, "invalid_settings", err.Error(), false)
			return
		}
		if err := a.store.SaveStorageSettings(r.Context(), request); err != nil {
			if errors.Is(err, monitoring.ErrSettingsConflict) {
				writeFleetError(w, 409, "settings_conflict", err.Error(), true)
			} else {
				writeFleetError(w, 503, "storage_unavailable", "storage settings could not be saved", true)
			}
			return
		}
		result, err := a.store.StorageStatus(r.Context())
		if err != nil {
			writeFleetError(w, 503, "storage_unavailable", "settings saved but status could not be loaded", true)
			return
		}
		writeStorageJSON(w, 200, result)
	default:
		writeFleetError(w, 405, "method_not_allowed", "method not allowed", false)
	}
}
func (a *API) storageNotifications(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		items, err := a.store.StorageNotifications(r.Context())
		if err != nil {
			writeFleetError(w, 503, "notifications_unavailable", "notifications could not be loaded", true)
			return
		}
		writeStorageJSON(w, 200, struct {
			Items []monitoring.StorageNotification `json:"items"`
		}{items})
	case http.MethodPost:
		if err := a.store.MarkStorageNotificationsRead(r.Context()); err != nil {
			writeFleetError(w, 503, "notifications_unavailable", "notifications could not be marked read", true)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeFleetError(w, 405, "method_not_allowed", "method not allowed", false)
	}
}

func writeStorageJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
