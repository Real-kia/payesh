package fleet

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/install"
	"github.com/Real-kia/payesh/internal/monitoring"
)

const (
	sshInstallJobKind            = "install-ssh"
	maxInstallSecrets            = 64
	defaultInstallTTL            = 30 * time.Minute
	defaultInstallWorkerInterval = 5 * time.Second
)

var (
	errInstallCredentialHandoffFull = errors.New("installation credential handoff is full")
	errInstallExecutorUnavailable   = errors.New("SSH installation executor is not configured")
)

// SSHInstaller is injectable so the durable binding can be exercised without
// invoking OpenSSH. InstallOverSSH remains the production implementation.
type SSHInstaller interface {
	Install(context.Context, install.SSHInstallOptions) (install.SSHInstallResult, error)
}

type SSHInstallerFunc func(context.Context, install.SSHInstallOptions) (install.SSHInstallResult, error)

func (f SSHInstallerFunc) Install(ctx context.Context, opts install.SSHInstallOptions) (install.SSHInstallResult, error) {
	return f(ctx, opts)
}

// InstallService binds owner intent to a durable SSH installation job. The
// job row contains only non-secret target metadata. Passwords, keys, and
// passphrases live in pending only until the worker consumes the job and are
// never serialized, logged, or included in an idempotency hash.
type InstallService struct {
	Store    *monitoring.Store
	Executor SSHInstaller
	Now      func() time.Time
	// Static verified artifacts and callbacks are process configuration, not
	// request data. Request-provided paths are intentionally not accepted.
	InstallerPath      string
	Artifacts          map[string]string
	VerifyArtifact     func(name, path string) error
	Enroll             func(context.Context, contracts.ServerID) error
	VerifyMeasurements func(context.Context, contracts.ServerID) error
	Transport          install.SSHTransport
	ConfirmHostKey     func(install.SSHHostKey) bool
	KnownHostsPath     string

	mu      sync.Mutex
	pending map[string]install.SSHInstallOptions
}

func NewInstallService(store *monitoring.Store) (*InstallService, error) {
	if store == nil {
		return nil, errors.New("install service requires store")
	}
	return &InstallService{Store: store, pending: make(map[string]install.SSHInstallOptions)}, nil
}

func (s *InstallService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *InstallService) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

type installRequest struct {
	ServerID                   string     `json:"server_id,omitempty"`
	Mode                       string     `json:"mode,omitempty"`
	Host                       string     `json:"host"`
	Port                       int        `json:"port"`
	User                       string     `json:"user"`
	Password                   string     `json:"password,omitempty"`
	PrivateKey                 string     `json:"private_key,omitempty"`
	PrivateKeyPassphrase       string     `json:"private_key_passphrase,omitempty"`
	SudoPassword               string     `json:"sudo_password,omitempty"`
	ExpectedHostKeyFingerprint string     `json:"expected_host_key_fingerprint,omitempty"`
	Role                       string     `json:"role,omitempty"`
	Listen                     string     `json:"listen,omitempty"`
	Start                      bool       `json:"start,omitempty"`
	IdempotencyKey             string     `json:"idempotency_key"`
	ExpiresAt                  *time.Time `json:"expires_at,omitempty"`
}

func (s *InstallService) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeFleetError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", false)
		return
	}
	pathID, pathOK := installPathServerID(r.URL.Path)
	if r.URL.Path != "/api/v1/installations" && !pathOK {
		writeFleetError(w, http.StatusNotFound, "not_found", "resource not found", false)
		return
	}
	var request installRequest
	if !decode(w, r, &request) {
		return
	}
	if pathOK {
		if request.ServerID != "" && request.ServerID != string(pathID) {
			writeFleetError(w, http.StatusBadRequest, "invalid_request", "server_id does not match the URL", false)
			return
		}
		request.ServerID = string(pathID)
	}
	job, err := s.Enqueue(r.Context(), request)
	if err != nil {
		s.writeEnqueueError(w, err)
		return
	}
	writeJob(w, http.StatusAccepted, job)
}

func installPathServerID(path string) (contracts.ServerID, bool) {
	const prefix, suffix = "/api/v1/servers/", "/install"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	if len(raw) < 16 || len(raw) > 128 || !safeFleetID(raw) {
		return "", false
	}
	return contracts.ServerID(raw), true
}

func (s *InstallService) Enqueue(ctx context.Context, request installRequest) (contracts.Job, error) {
	if s == nil || s.Store == nil {
		return contracts.Job{}, errors.New("install service is unavailable")
	}
	if s.Executor == nil {
		return contracts.Job{}, errInstallExecutorUnavailable
	}
	if request.Mode != "" && request.Mode != "ssh" || len(request.ServerID) < 16 || len(request.ServerID) > 128 || !safeFleetID(request.ServerID) ||
		len(request.IdempotencyKey) == 0 || len(request.IdempotencyKey) > 128 {
		return contracts.Job{}, errors.New("invalid installation request")
	}
	if strings.TrimSpace(request.Host) == "" || request.Port < 1 || request.Port > 65535 || strings.TrimSpace(request.User) == "" || len(request.Host) > 255 || len(request.User) > 128 {
		return contracts.Job{}, errors.New("invalid SSH target")
	}
	if len(request.Password) > 1<<20 || len(request.PrivateKey) > 1<<20 || len(request.PrivateKeyPassphrase) > 4096 || len(request.SudoPassword) > 4096 {
		return contracts.Job{}, errors.New("SSH credentials exceed the request limit")
	}
	if (request.Password == "") == (request.PrivateKey == "") {
		return contracts.Job{}, errors.New("exactly one SSH authentication method is required")
	}
	now := s.now()
	expires := now.Add(defaultInstallTTL)
	if request.ExpiresAt != nil {
		expires = request.ExpiresAt.UTC()
	}
	serverID := contracts.ServerID(request.ServerID)
	if _, found, err := s.Store.GetServer(ctx, serverID); err != nil {
		return contracts.Job{}, fmt.Errorf("installation target lookup: %w", err)
	} else if !found {
		return contracts.Job{}, errors.New("installation target not found")
	}
	existing, storedHash, existingFound, err := s.Store.GetJobByOperation(ctx, sshInstallJobKind, serverID, request.IdempotencyKey)
	if err != nil {
		return contracts.Job{}, err
	}
	if existingFound && request.ExpiresAt == nil {
		// Match update producer semantics: retries without an explicit expiry
		// retain the original authorization window instead of hashing `now`.
		expires = existing.ExpiresAt
	}
	// Hash only non-secret request fields. Keeping a credential digest in the
	// database would create a durable password-derived oracle.
	hashInput := fmt.Sprintf("%s\x00%s\x00%d\x00%s\x00%s\x00%s\x00%s\x00%s\x00%t\x00%s", serverID, request.Mode, request.Port, request.Host, request.User, request.Role, request.Listen, request.ExpectedHostKeyFingerprint, request.Start, expires.Format(time.RFC3339Nano))
	digest := sha256.Sum256([]byte(hashInput))
	requestHash := hex.EncodeToString(digest[:])
	if existingFound {
		if storedHash != requestHash {
			return contracts.Job{}, monitoring.ErrJobIdempotencyConflict
		}
		if existing.State == contracts.JobQueued && !savePendingInstall(s, existing.ID, request) {
			return contracts.Job{}, errInstallCredentialHandoffFull
		}
		return existing, nil
	}
	id, err := newInstallJobID()
	if err != nil {
		return contracts.Job{}, err
	}
	job, _, err := s.Store.CreateJob(ctx, contracts.Job{ID: id, Kind: sshInstallJobKind, State: contracts.JobQueued, IdempotencyKey: request.IdempotencyKey, TargetServerID: serverID, ExpiresAt: expires}, requestHash, now)
	if err != nil {
		if existing, storedHash, found, lookupErr := s.Store.GetJobByOperation(ctx, sshInstallJobKind, serverID, request.IdempotencyKey); lookupErr == nil && found && storedHash == requestHash {
			if existing.State == contracts.JobQueued && !savePendingInstall(s, existing.ID, request) {
				return contracts.Job{}, errInstallCredentialHandoffFull
			}
			return existing, nil
		}
		return contracts.Job{}, err
	}
	if !savePendingInstall(s, job.ID, request) {
		return contracts.Job{}, errInstallCredentialHandoffFull
	}
	return job, nil
}

func savePendingInstall(s *InstallService, id string, request installRequest) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		s.pending = make(map[string]install.SSHInstallOptions)
	}
	if _, exists := s.pending[id]; !exists && len(s.pending) >= maxInstallSecrets {
		return false
	}
	if previous, exists := s.pending[id]; exists {
		clear(previous.Auth.Password)
		clear(previous.Auth.PrivateKey)
		clear(previous.Auth.PrivateKeyPassphrase)
		clear(previous.Auth.SudoPassword)
	}
	s.pending[id] = s.optionsFor(request)
	return true
}

func (s *InstallService) optionsFor(request installRequest) install.SSHInstallOptions {
	return install.SSHInstallOptions{
		Endpoint:       install.SSHEndpoint{Host: request.Host, Port: request.Port, User: request.User},
		KnownHostsPath: s.KnownHostsPath, ExpectedHostKeyFingerprint: request.ExpectedHostKeyFingerprint,
		ConfirmHostKey: s.ConfirmHostKey,
		Auth:           install.SSHAuth{Password: []byte(request.Password), PrivateKey: []byte(request.PrivateKey), PrivateKeyPassphrase: []byte(request.PrivateKeyPassphrase), SudoPassword: []byte(request.SudoPassword)},
		InstallerPath:  s.InstallerPath, Artifacts: cloneStrings(s.Artifacts), Role: request.Role, Listen: request.Listen, Start: request.Start,
		VerifyArtifact: s.VerifyArtifact, Transport: s.Transport,
		Enroll: func(ctx context.Context) error {
			if s.Enroll == nil {
				return install.ErrSSHEnrollmentRequired
			}
			return s.Enroll(ctx, contracts.ServerID(request.ServerID))
		},
		VerifyMeasurements: func(ctx context.Context) error {
			if s.VerifyMeasurements == nil {
				return install.ErrSSHMeasurementsRequired
			}
			return s.VerifyMeasurements(ctx, contracts.ServerID(request.ServerID))
		},
	}
}

func cloneStrings(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

// RunOnce executes one durable installation. Credentials are removed from the
// pending map before the external call, so a replay cannot race or overwrite
// buffers being used by SSH. A running job remains visible after interruption;
// a later pass records a retryable failure because credentials are one-shot.
func (s *InstallService) RunOnce(ctx context.Context, id string) (contracts.Job, error) {
	if s == nil || s.Store == nil || id == "" {
		return contracts.Job{}, errors.New("install service and job are required")
	}
	job, found, err := s.Store.GetJob(ctx, id)
	if err != nil {
		return contracts.Job{}, err
	}
	if !found || job.Kind != sshInstallJobKind {
		return contracts.Job{}, errors.New("installation job not found")
	}
	if job.State == contracts.JobSucceeded || job.State == contracts.JobFailed || job.State == contracts.JobCancelled || job.State == contracts.JobRecoveryRequired {
		return job, nil
	}
	if job.CancelRequested || job.State == contracts.JobCancelling {
		cancelled, transitionErr := s.Store.TransitionJob(ctx, id, job.Revision, contracts.JobCancelled, 100, nil, s.now())
		if transitionErr == nil {
			s.clearPending(id)
		}
		return cancelled, transitionErr
	}
	if !job.ExpiresAt.After(s.now()) {
		return s.fail(ctx, job, "expired", "installation authorization expired", false)
	}
	if s.Executor == nil {
		return s.fail(ctx, job, "executor_unavailable", errInstallExecutorUnavailable.Error(), true)
	}
	if job.State == contracts.JobQueued {
		job, err = s.Store.TransitionJob(ctx, id, job.Revision, contracts.JobRunning, 5, nil, s.now())
		if err != nil {
			return contracts.Job{}, err
		}
	}
	s.mu.Lock()
	opts, ok := s.pending[id]
	if ok {
		delete(s.pending, id)
	}
	s.mu.Unlock()
	if !ok || (len(opts.Auth.Password) == 0 && len(opts.Auth.PrivateKey) == 0) {
		return s.fail(ctx, job, "credentials_unavailable", "installation credentials are no longer available; resubmit the installation", true)
	}
	defer clearInstallAuth(&opts.Auth)
	result, execErr := s.Executor.Install(ctx, opts)
	if errors.Is(execErr, context.Canceled) || errors.Is(execErr, context.DeadlineExceeded) {
		return job, execErr
	}
	if execErr != nil {
		message := "SSH installation failed"
		if stage := install.SSHInstallFailureStage(execErr); stage != "" {
			message += " during " + stage
		}
		return s.fail(ctx, job, "install_failed", message, true)
	}
	if server, found, lookupErr := s.Store.GetServer(ctx, job.TargetServerID); lookupErr == nil && found {
		server.Address = opts.Endpoint.Host
		server.Platform = "linux"
		if result.Preflight.Architecture != "" {
			server.Architecture = result.Preflight.Architecture
		}
		if updateErr := s.Store.UpsertServer(ctx, server); updateErr != nil {
			return s.fail(ctx, job, "inventory_update_failed", "installation completed but detected server inventory could not be saved", true)
		}
	}
	completed, err := s.Store.TransitionJob(ctx, id, job.Revision, contracts.JobSucceeded, 100, nil, s.now())
	if err == nil {
		s.clearPending(id)
	}
	return completed, err
}

// ProcessPending advances every durable installation at most once. It is
// deliberately serialized with itself by StartWorker and bounded by the
// store's job history limit.
func (s *InstallService) ProcessPending(ctx context.Context) error {
	if s == nil || s.Store == nil {
		return errors.New("install service store is required")
	}
	jobs, err := s.Store.ListJobsByKind(ctx, sshInstallJobKind)
	if err != nil {
		return err
	}
	var firstErr error
	for _, job := range jobs {
		if job.State == contracts.JobSucceeded || job.State == contracts.JobFailed || job.State == contracts.JobCancelled || job.State == contracts.JobRecoveryRequired {
			continue
		}
		if _, runErr := s.RunOnce(ctx, job.ID); runErr != nil && ctx.Err() != nil {
			return ctx.Err()
		} else if runErr != nil && firstErr == nil {
			firstErr = runErr
		}
	}
	return firstErr
}

// StartWorker runs bounded installation recovery passes until ctx is
// cancelled. A nil executor does not leave durable work queued forever:
// submissions are rejected and pre-existing rows settle via RunOnce.
func (s *InstallService) StartWorker(ctx context.Context, interval time.Duration, onError func(error)) <-chan struct{} {
	done := make(chan struct{})
	if ctx == nil {
		ctx = context.Background()
	}
	if interval <= 0 || interval > time.Hour {
		interval = defaultInstallWorkerInterval
	}
	go func() {
		defer close(done)
		run := func() {
			if err := s.ProcessPending(ctx); err != nil && ctx.Err() == nil && onError != nil {
				onError(err)
			}
		}
		run()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run()
			}
		}
	}()
	return done
}

func (s *InstallService) fail(ctx context.Context, job contracts.Job, code, message string, retryable bool) (contracts.Job, error) {
	failed, err := s.Store.TransitionJob(ctx, job.ID, job.Revision, contracts.JobFailed, 100, &contracts.Error{Code: code, Message: message, Retryable: retryable}, s.now())
	if err == nil {
		s.clearPending(job.ID)
	}
	return failed, err
}

func (s *InstallService) clearPending(id string) {
	s.mu.Lock()
	if opts, found := s.pending[id]; found {
		clear(opts.Auth.Password)
		clear(opts.Auth.PrivateKey)
		clear(opts.Auth.PrivateKeyPassphrase)
		clear(opts.Auth.SudoPassword)
		delete(s.pending, id)
	}
	s.mu.Unlock()
}

func clearInstallAuth(auth *install.SSHAuth) {
	if auth == nil {
		return
	}
	clear(auth.Password)
	clear(auth.PrivateKey)
	clear(auth.PrivateKeyPassphrase)
	clear(auth.SudoPassword)
}

func (s *InstallService) writeEnqueueError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errInstallExecutorUnavailable):
		writeFleetError(w, http.StatusServiceUnavailable, "install_executor_unavailable", "SSH installation is not configured", true)
	case errors.Is(err, errInstallCredentialHandoffFull):
		writeFleetError(w, http.StatusServiceUnavailable, "credential_handoff_full", "too many pending SSH installations", true)
	case errors.Is(err, monitoring.ErrJobIdempotencyConflict):
		writeFleetError(w, http.StatusConflict, "idempotency_conflict", "idempotency key was already used for different input", false)
	case strings.Contains(err.Error(), "not found"):
		writeFleetError(w, http.StatusNotFound, "not_found", "installation target not found", false)
	case strings.Contains(err.Error(), "storage"):
		writeFleetError(w, http.StatusServiceUnavailable, "job_storage_unavailable", "job storage unavailable", true)
	default:
		writeFleetError(w, http.StatusBadRequest, "invalid_request", err.Error(), false)
	}
}

func newInstallJobID() (string, error) {
	var raw [12]byte
	if _, err := cryptorand.Read(raw[:]); err != nil {
		return "", err
	}
	return "install-" + hex.EncodeToString(raw[:]), nil
}
