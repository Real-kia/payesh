package fleet

import (
	"net/http"
	"time"
)

func (a *API) fleetTraffic(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()
	switch r.URL.Path {
	case "/api/v1/fleet/traffic":
		if r.Method == http.MethodGet {
			history, err := a.store.FleetTrafficHistory(ctx, now)
			if err != nil {
				writeFleetError(w, 503, "traffic_unavailable", "fleet traffic history could not be loaded", true)
				return
			}
			writeStorageJSON(w, 200, history)
			return
		}
	case "/api/v1/settings/fleet-traffic":
		switch r.Method {
		case http.MethodGet:
			enabled, err := a.store.FleetTrafficEnabled(ctx)
			if err != nil {
				writeFleetError(w, 503, "traffic_unavailable", "fleet traffic settings could not be loaded", true)
				return
			}
			writeStorageJSON(w, 200, map[string]bool{"enabled": enabled})
			return
		case http.MethodPut:
			principal, ok := a.sessions.Authenticate(r)
			if !ok || principal.Role != "owner" {
				writeFleetError(w, 403, "owner_required", "only the owner can change fleet traffic settings", false)
				return
			}
			var request struct {
				Enabled *bool `json:"enabled"`
			}
			if !decode(w, r, &request) {
				return
			}
			if request.Enabled == nil {
				writeFleetError(w, 400, "invalid_settings", "enabled is required", false)
				return
			}
			if err := a.store.SetFleetTrafficEnabled(ctx, *request.Enabled, now); err != nil {
				writeFleetError(w, 503, "traffic_unavailable", "fleet traffic settings could not be saved", true)
				return
			}
			writeStorageJSON(w, 200, map[string]bool{"enabled": *request.Enabled})
			return
		}
	}
	writeFleetError(w, 405, "method_not_allowed", "method not allowed", false)
}
