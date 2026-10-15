package fleet

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/Real-kia/payesh/internal/monitoring"
)

type tagsRequest struct {
	Tags []string `json:"tags"`
}

// tagsHandler serves PUT /api/v1/servers/{id}/tags, replacing the server's
// tags (behind the session/token middleware, which requires edit access).
func tagsHandler(store *monitoring.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.Header().Set("Allow", http.MethodPut)
			writeFleetError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
			return
		}
		serverID, ok := serverIDForSuffix(r.URL.Path, "/tags")
		if !ok {
			writeFleetError(w, http.StatusNotFound, "not_found", "server not found", false)
			return
		}
		var request tagsRequest
		if !decode(w, r, &request) {
			return
		}
		tags, err := store.SetServerTags(r.Context(), serverID, request.Tags)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			writeFleetError(w, http.StatusNotFound, "not_found", "server not found", false)
		case errors.Is(err, monitoring.ErrInvalidTags):
			writeFleetError(w, http.StatusBadRequest, "invalid_tags", err.Error(), false)
		case err != nil:
			writeFleetError(w, http.StatusServiceUnavailable, "storage_error", "could not save tags", true)
		default:
			writeStorageJSON(w, http.StatusOK, tagsRequest{Tags: tags})
		}
	})
}
