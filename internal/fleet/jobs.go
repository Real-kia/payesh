package fleet

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

type jobHTTP struct{ store *monitoring.Store }

func newJobHTTP(store *monitoring.Store) http.Handler { return &jobHTTP{store: store} }

func (h *jobHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(strings.TrimRight(r.URL.Path, "/"), "/api/v1/jobs/")
	if id == "" || strings.Contains(id, "/") || len(id) > 128 {
		writeFleetError(w, http.StatusNotFound, "not_found", "job not found", false)
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.get(w, r, id)
	case http.MethodPost:
		h.cancel(w, r, id)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeFleetError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
	}
}

func (h *jobHTTP) get(w http.ResponseWriter, r *http.Request, id string) {
	job, found, err := h.store.GetJob(r.Context(), id)
	if err != nil {
		writeFleetError(w, http.StatusServiceUnavailable, "job_storage_unavailable", "job storage unavailable", true)
		return
	}
	if !found {
		writeFleetError(w, http.StatusNotFound, "not_found", "job not found", false)
		return
	}
	writeJob(w, http.StatusOK, job)
}

type cancelJobRequest struct {
	IdempotencyKey   string          `json:"idempotency_key"`
	ExpectedRevision json.RawMessage `json:"expected_revision"`
}

func (h *jobHTTP) cancel(w http.ResponseWriter, r *http.Request, id string) {
	var request cancelJobRequest
	if !decode(w, r, &request) {
		return
	}
	if request.IdempotencyKey == "" || len(request.IdempotencyKey) > 128 {
		writeFleetError(w, http.StatusBadRequest, "invalid_request", "invalid idempotency_key", false)
		return
	}
	expected, err := parseJobRevision(request.ExpectedRevision)
	if err != nil {
		writeFleetError(w, http.StatusBadRequest, "invalid_request", "expected_revision must be an unsigned decimal string", false)
		return
	}
	job, err := h.store.RequestJobCancellation(r.Context(), id, request.IdempotencyKey, expected, time.Now().UTC())
	switch {
	case err == nil:
		writeJob(w, http.StatusAccepted, job)
	case errors.Is(err, monitoring.ErrJobNotFound):
		writeFleetError(w, http.StatusNotFound, "not_found", "job not found", false)
	case errors.Is(err, monitoring.ErrJobRevisionConflict):
		writeFleetError(w, http.StatusConflict, "job_revision_conflict", "job changed; reload before cancelling", false)
	case errors.Is(err, monitoring.ErrJobNotCancellable):
		writeFleetError(w, http.StatusConflict, "job_not_cancellable", "job is already complete", false)
	case errors.Is(err, monitoring.ErrJobIdempotencyConflict):
		writeFleetError(w, http.StatusConflict, "idempotency_conflict", "idempotency key was already used for different input", false)
	default:
		writeFleetError(w, http.StatusServiceUnavailable, "job_storage_unavailable", "job storage unavailable", true)
	}
}

func parseJobRevision(raw json.RawMessage) (uint64, error) {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil || value == "" {
		return 0, errors.New("invalid revision")
	}
	return strconv.ParseUint(value, 10, 64)
}

func writeJob(w http.ResponseWriter, status int, job contracts.Job) {
	data, err := json.Marshal(job)
	if err != nil {
		writeFleetError(w, http.StatusInternalServerError, "internal_error", "request failed", false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}
