package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Real-kia/payesh/internal/auth"
	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/release"
	"github.com/Real-kia/payesh/internal/updater"
	"github.com/Real-kia/payesh/internal/version"
	"github.com/Real-kia/payesh/internal/webupdate"
)

const defaultUpdateAuthorizationTTL = 30 * time.Minute

// UpdateService is the browser-side update producer. It contains no worker
// loop: workers call Scheduler.RunOnce/Run against the durable job ID. This
// keeps owner intent and execution separate and makes a process restart safe.
type UpdateService struct {
	Store     *monitoring.Store
	Scheduler *updater.Scheduler
	Now       func() time.Time
	// Sessions identifies the owner for browser-triggered self-updates.
	Sessions *auth.Manager
	// WebUpdateDir and WebUpdateRoot locate the root worker's handoff files and
	// service definition; empty values use the installed defaults.
	WebUpdateDir   string
	WebUpdateRoot  string
	ReleaseChecker interface {
		Latest(context.Context) (release.GitHubRelease, error)
	}
}

func NewUpdateService(store *monitoring.Store, scheduler *updater.Scheduler) (*UpdateService, error) {
	if store == nil || scheduler == nil || scheduler.Store != store {
		return nil, errors.New("update service requires a matching store and scheduler")
	}
	return &UpdateService{Store: store, Scheduler: scheduler}, nil
}

func (s *UpdateService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *UpdateService) webUpdateDir() string {
	if s.WebUpdateDir != "" {
		return s.WebUpdateDir
	}
	return webupdate.DefaultDir
}

func (s *UpdateService) webUpdateRoot() string {
	if s.WebUpdateRoot != "" {
		return s.WebUpdateRoot
	}
	return "/"
}

// webUpdateStatus returns the current worker state, hiding abandoned work.
func (s *UpdateService) webUpdateStatus() *webupdate.Status {
	status, err := webupdate.ReadStatus(s.webUpdateDir())
	if err != nil {
		return nil
	}
	if (status.State == webupdate.StateQueued || status.State == webupdate.StateRunning) && !status.Active(s.now()) {
		status.State, status.Message = webupdate.StateFailed, "update did not finish"
	}
	return &status
}

func (s *UpdateService) serveApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeFleetError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
		return
	}
	if s.Sessions == nil {
		writeFleetError(w, http.StatusNotFound, "not_found", "resource not found", false)
		return
	}
	account, ok := s.Sessions.Authenticate(r)
	if !ok {
		writeFleetError(w, http.StatusUnauthorized, "unauthorized", "authentication required", true)
		return
	}
	if account.Role != "owner" {
		writeFleetError(w, http.StatusForbidden, "owner_required", "owner access required", false)
		return
	}
	if !webupdate.Supported(s.webUpdateRoot()) {
		writeFleetError(w, http.StatusConflict, "web_update_unavailable", "web updates are not set up on this server; run sudo payesh update once from the command line", false)
		return
	}
	var request struct {
		Version string `json:"version"`
	}
	if !decode(w, r, &request) {
		return
	}
	target := strings.TrimPrefix(strings.TrimSpace(request.Version), "v")
	if len(target) > 64 || !updater.ValidRelease(target) {
		writeFleetError(w, http.StatusBadRequest, "invalid_request", "version is invalid", false)
		return
	}
	if updater.ValidRelease(version.Value) {
		if cmp, err := updater.CompareReleases(version.Value, target); err == nil && cmp >= 0 {
			writeFleetError(w, http.StatusConflict, "not_newer", "the requested version is not newer than the installed version", false)
			return
		}
	}
	if err := webupdate.Submit(s.webUpdateDir(), webupdate.Request{Version: target, RequestedBy: account.Username}, s.now()); err != nil {
		if errors.Is(err, webupdate.ErrBusy) {
			writeFleetError(w, http.StatusConflict, "update_in_progress", "an update is already in progress", true)
		} else {
			writeFleetError(w, http.StatusServiceUnavailable, "update_unavailable", "could not queue the update", true)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(s.webUpdateStatus())
}

func (s *UpdateService) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

type updateRequest struct {
	Release           string     `json:"release"`
	SelectedServerIDs []string   `json:"selected_server_ids"`
	IdempotencyKey    string     `json:"idempotency_key"`
	ExpectedRevision  *uint64    `json:"expected_revision,omitempty,string"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
}

func (s *UpdateService) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/v1/updates/apply" {
		s.serveApply(w, r)
		return
	}
	if r.URL.Path == "/api/v1/updates/status" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeFleetError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Current            string            `json:"current"`
			WebUpdateSupported bool              `json:"web_update_supported"`
			WebUpdate          *webupdate.Status `json:"web_update"`
		}{version.Value, webupdate.Supported(s.webUpdateRoot()), s.webUpdateStatus()})
		return
	}
	if r.URL.Path == "/api/v1/updates/latest" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeFleetError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
			return
		}
		checker := s.ReleaseChecker
		if checker == nil {
			checker = release.GitHubClient{Token: os.Getenv("GITHUB_TOKEN")}
		}
		latest, err := checker.Latest(r.Context())
		if err != nil {
			writeFleetError(w, http.StatusBadGateway, "release_check_failed", "could not check GitHub Releases; for a private repository configure GITHUB_TOKEN", true)
			return
		}
		available, ahead := true, false
		if updater.ValidRelease(version.Value) {
			if comparison, err := updater.CompareReleases(version.Value, latest.Version); err == nil {
				available = comparison < 0
				ahead = comparison > 0
			}
		}
		var history []release.GitHubRelease
		if historian, ok := checker.(interface {
			History(context.Context) ([]release.GitHubRelease, error)
		}); ok {
			history, _ = historian.History(r.Context())
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Current            string                  `json:"current"`
			Latest             string                  `json:"latest"`
			InstalledAhead     bool                    `json:"installed_ahead"`
			UpdateAvailable    bool                    `json:"update_available"`
			URL                string                  `json:"url"`
			Releases           []release.GitHubRelease `json:"releases"`
			WebUpdateSupported bool                    `json:"web_update_supported"`
			WebUpdate          *webupdate.Status       `json:"web_update"`
		}{version.Value, latest.Version, ahead, available, latest.URL, history, webupdate.Supported(s.webUpdateRoot()), s.webUpdateStatus()})
		return
	}
	if r.URL.Path != "/api/v1/updates" {
		writeFleetError(w, http.StatusNotFound, "not_found", "resource not found", false)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeFleetError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
		return
	}
	if s.Sessions == nil {
		writeFleetError(w, http.StatusNotFound, "not_found", "resource not found", false)
		return
	}
	account, ok := s.Sessions.Authenticate(r)
	if !ok {
		writeFleetError(w, http.StatusUnauthorized, "unauthorized", "authentication required", true)
		return
	}
	if account.Role != "owner" {
		writeFleetError(w, http.StatusForbidden, "owner_required", "owner access required", false)
		return
	}
	var request updateRequest
	if !decode(w, r, &request) {
		return
	}
	if strings.TrimSpace(request.Release) == "" || len(request.Release) > 64 || !updater.ValidRelease(request.Release) ||
		len(request.IdempotencyKey) == 0 || len(request.IdempotencyKey) > 128 ||
		len(request.SelectedServerIDs) == 0 || len(request.SelectedServerIDs) > 20 {
		writeFleetError(w, http.StatusBadRequest, "invalid_request", "release, selected_server_ids, and idempotency_key are invalid", false)
		return
	}
	now := s.now()
	expires := now.Add(defaultUpdateAuthorizationTTL)
	if request.ExpiresAt != nil {
		expires = request.ExpiresAt.UTC()
	}
	// An omitted expiry is generated by the service, but replays must hash to
	// the original operation. Reuse the durable expiry when the key exists.
	if existing, _, found, err := s.Store.GetJobByOperation(r.Context(), updater.UpdateJobKind, "", request.IdempotencyKey); err != nil {
		writeFleetError(w, http.StatusServiceUnavailable, "job_storage_unavailable", "could not read update job history", true)
		return
	} else if found && request.ExpiresAt == nil {
		expires = existing.ExpiresAt
	}
	seen := make(map[contracts.ServerID]struct{}, len(request.SelectedServerIDs))
	targets := make([]updater.Target, 0, len(request.SelectedServerIDs))
	for _, rawID := range request.SelectedServerIDs {
		if len(rawID) < 16 || len(rawID) > 128 || !safeFleetID(rawID) {
			writeFleetError(w, http.StatusBadRequest, "invalid_request", "selected server ID is invalid", false)
			return
		}
		serverID := contracts.ServerID(rawID)
		if _, duplicate := seen[serverID]; duplicate {
			writeFleetError(w, http.StatusBadRequest, "invalid_request", "selected server IDs must be unique", false)
			return
		}
		seen[serverID] = struct{}{}
		server, found, err := s.Store.GetServer(r.Context(), serverID)
		if err != nil {
			writeFleetError(w, http.StatusServiceUnavailable, "storage_unavailable", "could not read selected server", true)
			return
		}
		if !found {
			writeFleetError(w, http.StatusNotFound, "not_found", "selected server not found", false)
			return
		}
		targets = append(targets, updater.Target{Server: server, Compatible: true})
	}
	job, err := s.Scheduler.ScheduleUpdate(r.Context(), updater.Plan{Release: request.Release, IdempotencyKey: request.IdempotencyKey, Targets: targets, ExpiresAt: expires})
	if err != nil {
		switch {
		case errors.Is(err, monitoring.ErrJobIdempotencyConflict):
			writeFleetError(w, http.StatusConflict, "idempotency_conflict", "idempotency key was already used for different input", false)
		case errors.Is(err, monitoring.ErrStoragePressure):
			writeFleetError(w, http.StatusServiceUnavailable, "storage_pressure", "job storage is full", true)
		case strings.HasPrefix(err.Error(), "updater: invalid update plan"):
			writeFleetError(w, http.StatusBadRequest, "invalid_request", "update plan is invalid or expired", false)
		default:
			writeFleetError(w, http.StatusServiceUnavailable, "job_storage_unavailable", "could not create update job", true)
		}
		return
	}
	writeJob(w, http.StatusAccepted, job)
}

func safeFleetID(value string) bool {
	for _, ch := range value {
		if !(ch == '-' || ch == '_' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9') {
			return false
		}
	}
	return true
}
