package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/exec"
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
	WebUpdateDir      string
	WebUpdateRoot     string
	SharedUpdateScope bool
	// InstalledVersion reports the release installed on disk; Restart makes the
	// process exit so its service manager starts the installed binary. Both
	// default to the shared /usr/bin/payesh and a delayed non-zero exit.
	InstalledVersion func(context.Context) string
	Restart          func()
	ReleaseChecker   interface {
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

// webUpdateAvailable requires the managed instance and a writable queue.
func (s *UpdateService) webUpdateAvailable() bool {
	if s.SharedUpdateScope || !webupdate.Supported(s.webUpdateRoot()) {
		return false
	}
	probe, err := os.CreateTemp(s.webUpdateDir(), ".write-check-")
	if err != nil {
		return false
	}
	_ = probe.Close()
	_ = os.Remove(probe.Name())
	return true
}

// restartPending reports the on-disk release when this hub cannot use the root
// worker (a second install sharing the host's binaries) and that release is
// not the one running, so a restart would apply it.
func (s *UpdateService) restartPending(ctx context.Context) string {
	if s.webUpdateAvailable() || !updater.ValidRelease(version.Value) {
		return ""
	}
	lookup := s.InstalledVersion
	if lookup == nil {
		lookup = installedBinaryVersion
	}
	installed := strings.TrimPrefix(strings.TrimSpace(lookup(ctx)), "v")
	if !updater.ValidRelease(installed) {
		return ""
	}
	if cmp, err := updater.CompareReleases(version.Value, installed); err != nil || cmp >= 0 {
		return ""
	}
	return installed
}

func installedBinaryVersion(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/payesh", "version").Output()
	if err != nil || len(out) > 128 {
		return ""
	}
	return string(out)
}

func (s *UpdateService) serveRestart(w http.ResponseWriter, r *http.Request) {
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
	target := s.restartPending(r.Context())
	if target == "" {
		writeFleetError(w, http.StatusConflict, "restart_not_needed", "no newer installed release is waiting for a restart", false)
		return
	}
	restart := s.Restart
	if restart == nil {
		restart = func() { time.Sleep(time.Second); os.Exit(75) }
	}
	go restart()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"restarting_into": target})
}

func (s *UpdateService) webUpdateStatus() *webupdate.Status {
	if s.SharedUpdateScope {
		return nil
	}
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
	if s.SharedUpdateScope {
		writeFleetError(w, http.StatusConflict, "shared_update_scope", "This instance shares its binaries. Update the managed installation and restart this instance.", false)
		return
	}
	if !s.webUpdateAvailable() {
		writeFleetError(w, http.StatusConflict, "web_update_unavailable", "The local update worker or its queue is unavailable. Check the worker setup or update from the command line.", false)
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
			log.Printf("web update queue failed for release %s: %v", target, err)
			writeFleetError(w, http.StatusServiceUnavailable, "update_unavailable", "could not queue the update; check the hub service log for details", true)
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
	Tag               *string    `json:"tag,omitempty"`
	updater.RolloutOptions
}

func (s *UpdateService) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(strings.TrimRight(r.URL.Path, "/"), "/resume") {
		s.serveResume(w, r)
		return
	}
	if r.URL.Path == "/api/v1/updates/apply" {
		s.serveApply(w, r)
		return
	}
	if r.URL.Path == "/api/v1/updates/restart" {
		s.serveRestart(w, r)
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
			SharedUpdateScope  bool              `json:"shared_update_scope,omitempty"`
			WebUpdateSupported bool              `json:"web_update_supported"`
			WebUpdate          *webupdate.Status `json:"web_update"`
			RestartTo          string            `json:"restart_available,omitempty"`
		}{version.Value, s.SharedUpdateScope, s.webUpdateAvailable(), s.webUpdateStatus(), s.restartPending(r.Context())})
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
		latestNode := latest.NodeVersion
		if latestNode == "" {
			latestNode = latest.Version
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
			LatestNode         string                  `json:"latest_node"`
			InstalledAhead     bool                    `json:"installed_ahead"`
			UpdateAvailable    bool                    `json:"update_available"`
			URL                string                  `json:"url"`
			Releases           []release.GitHubRelease `json:"releases"`
			SharedUpdateScope  bool                    `json:"shared_update_scope,omitempty"`
			WebUpdateSupported bool                    `json:"web_update_supported"`
			WebUpdate          *webupdate.Status       `json:"web_update"`
			RestartTo          string                  `json:"restart_available,omitempty"`
		}{version.Value, latest.Version, latestNode, ahead, available, latest.URL, history, s.SharedUpdateScope, s.webUpdateAvailable(), s.webUpdateStatus(), s.restartPending(r.Context())})
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
	if len(account.ServerIDs) > 0 {
		writeFleetError(w, http.StatusForbidden, "token_scope", "fleet rollouts require fleet-wide server access", false)
		return
	}
	var request updateRequest
	if !decode(w, r, &request) {
		return
	}
	if strings.TrimSpace(request.Release) == "" || len(request.Release) > 64 || !updater.ValidRelease(request.Release) ||
		len(request.IdempotencyKey) == 0 || len(request.IdempotencyKey) > 128 ||
		(request.SelectedServerIDs == nil) == (request.Tag == nil) ||
		(request.Tag == nil && len(request.SelectedServerIDs) == 0) || len(request.SelectedServerIDs) > updater.MaxRolloutTargets {
		writeFleetError(w, http.StatusBadRequest, "invalid_request", "provide a valid release, idempotency_key, and either selected_server_ids or tag", false)
		return
	}
	tag := ""
	if request.Tag != nil {
		tags, err := monitoring.NormalizeTags([]string{*request.Tag})
		if err != nil || len(tags) != 1 {
			writeFleetError(w, http.StatusBadRequest, "invalid_request", "tag is invalid", false)
			return
		}
		tag = strings.ToLower(tags[0])
	}
	if window := request.UpdateWindow; window != nil {
		window.Start, window.End = window.Start.UTC(), window.End.UTC()
	}
	now := s.now()
	defaultStart := now
	if window := request.UpdateWindow; window != nil && window.Start.After(now) {
		defaultStart = window.Start
	}
	expires := defaultStart.Add(defaultUpdateAuthorizationTTL)
	if request.ExpiresAt != nil {
		expires = request.ExpiresAt.UTC()
	}
	existing, _, found, err := s.Store.GetJobByOperation(r.Context(), updater.UpdateJobKind, "", request.IdempotencyKey)
	if err != nil {
		writeFleetError(w, http.StatusServiceUnavailable, "job_storage_unavailable", "could not read update job history", true)
		return
	}
	plan := updater.Plan{Release: request.Release, IdempotencyKey: request.IdempotencyKey, Tag: tag, ExpiresAt: expires, DefaultExpiry: request.ExpiresAt == nil, RolloutOptions: request.RolloutOptions}
	if tag != "" && !found {
		servers, err := s.Store.ServersWithTag(r.Context(), tag, updater.MaxRolloutTargets+1)
		if err != nil {
			writeFleetError(w, http.StatusServiceUnavailable, "storage_unavailable", "could not select servers by tag", true)
			return
		}
		for _, server := range servers {
			plan.Targets = append(plan.Targets, updater.Target{Server: server, Compatible: true})
		}
	}
	var replayTargets map[contracts.ServerID]updater.Target
	if found && tag == "" {
		replayTargets, err = s.Scheduler.ReplayTargets(r.Context(), existing.ID)
		if err != nil {
			writeFleetError(w, http.StatusServiceUnavailable, "job_storage_unavailable", "could not read update targets", true)
			return
		}
	}
	seen := make(map[contracts.ServerID]struct{}, len(request.SelectedServerIDs))
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
		if found {
			if replay, ok := replayTargets[serverID]; ok {
				plan.Targets = append(plan.Targets, replay)
				continue
			}
			plan.Targets = append(plan.Targets, updater.Target{Server: contracts.Server{ID: serverID}, Compatible: true})
			continue
		}
		server, found, err := s.Store.GetServer(r.Context(), serverID)
		if err != nil {
			writeFleetError(w, http.StatusServiceUnavailable, "storage_unavailable", "could not read selected server", true)
			return
		}
		if !found {
			writeFleetError(w, http.StatusNotFound, "not_found", "selected server not found", false)
			return
		}
		plan.Targets = append(plan.Targets, updater.Target{Server: server, Compatible: true})
	}
	job, err := s.Scheduler.ScheduleUpdate(r.Context(), plan)
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

func (s *UpdateService) serveResume(w http.ResponseWriter, r *http.Request) {
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
	if len(account.ServerIDs) > 0 {
		writeFleetError(w, http.StatusForbidden, "token_scope", "fleet rollouts require fleet-wide server access", false)
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(strings.TrimRight(r.URL.Path, "/"), "/api/v1/updates/"), "/resume")
	if id == "" || len(id) > 128 || !safeFleetID(id) {
		writeFleetError(w, http.StatusNotFound, "not_found", "job not found", false)
		return
	}
	var request struct {
		ExpectedRevision json.RawMessage `json:"expected_revision"`
	}
	if !decode(w, r, &request) {
		return
	}
	revision, err := parseJobRevision(request.ExpectedRevision)
	if err != nil {
		writeFleetError(w, http.StatusBadRequest, "invalid_request", "expected_revision must be an unsigned decimal string", false)
		return
	}
	job, err := s.Scheduler.ResumePaused(r.Context(), id, revision)
	switch {
	case err == nil:
		writeJob(w, http.StatusAccepted, job)
	case errors.Is(err, updater.ErrUpdateNotFound):
		writeFleetError(w, http.StatusNotFound, "not_found", "update job not found", false)
	case errors.Is(err, monitoring.ErrJobRevisionConflict):
		writeFleetError(w, http.StatusConflict, "job_revision_conflict", "job changed; reload before resuming", false)
	case errors.Is(err, updater.ErrUpdateNotPaused):
		writeFleetError(w, http.StatusConflict, "job_not_resumable", "update job is no longer resumable", false)
	default:
		writeFleetError(w, http.StatusServiceUnavailable, "job_storage_unavailable", "could not resume update job", true)
	}
}
