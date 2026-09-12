package modules

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/trust"
)

// DefaultHealthCheckTimeout bounds the post-install/post-enable capability
// check so a hung module process cannot block a lifecycle request forever.
const DefaultHealthCheckTimeout = 10 * time.Second

var ErrModuleExecutorUnavailable = errors.New("module_executor_unavailable")

// HealthCheckFunc runs a module's own bounded self-check after staging. The
// production server uses the typed payesh-privd module.invoke("health")
// executor when configured; tests and controlled integrations may inject a
// deterministic callback instead.
type HealthCheckFunc func(ctx context.Context, moduleID, installDir string) error

// ModuleInvocation is the narrow operation surface the server hands to the
// privileged helper. Implementations must not turn these fields into shell
// input; the in-tree privd client sends them as typed JSON.
type ModuleInvocation struct {
	ServerID      contracts.ServerID
	ModuleID      string
	ModuleVersion string
	Operation     string
	InstallDir    string
}

type ModuleExecutor interface {
	Invoke(ctx context.Context, invocation ModuleInvocation) error
}

// Manager implements the package-06 lifecycle: eligibility, trust-verified
// staging, atomic activation, and durable state transitions. It never
// executes an archive's contents itself and never accepts a request-provided
// download URL; RootDir only ever receives bytes this process already
// verified against a catalog-approved manifest.
type Manager struct {
	Store   *monitoring.Store
	Trust   *trust.Registry
	RootDir string
	// LocalServerID binds direct filesystem work to this installation. An empty
	// value fails closed; a fleet row's role label alone is not host identity.
	LocalServerID contracts.ServerID
	HealthCheck   HealthCheckFunc
	Executor      ModuleExecutor
	Now           func() time.Time
	// DeactivateHooks lets a control module (e.g. cpu-controls) register a
	// cleanup function run before Disable takes effect. Keyed by module ID;
	// a module with no registered hook disables with no extra cleanup step.
	DeactivateHooks map[string]func(ctx context.Context, serverID contracts.ServerID) error
	// LifecycleMu is shared with control managers so enable/disable/remove and
	// policy application cannot cross in the gap between host cleanup and the
	// durable module-state transition.
	LifecycleMu *sync.RWMutex
}

func (m *Manager) lockLifecycle() func() {
	if m.LifecycleMu == nil {
		return func() {}
	}
	m.LifecycleMu.Lock()
	return m.LifecycleMu.Unlock
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now().UTC()
}

func (m *Manager) installDir(serverID contracts.ServerID, moduleID string) string {
	return filepath.Join(m.RootDir, string(serverID), moduleID, "active")
}

func (m *Manager) stagingDir(serverID contracts.ServerID, moduleID, version string) string {
	return filepath.Join(m.RootDir, string(serverID), moduleID, "staged-"+version)
}

// InstallRequest carries everything an Install call needs to verify and
// stage one module release for one server. ManifestSignatureB64 signs the
// canonical JCS form of Manifest, per docs/contracts/RELEASE_FORMAT.md.
type InstallRequest struct {
	ServerID             contracts.ServerID
	ModuleID             string
	Manifest             contracts.ModuleManifest
	ManifestSignatureB64 string
	Archive              []byte
	ExpectedRevision     uint64
}

// Install runs the full eligibility → verify → stage → activate → health
// check sequence in one call, persisting a durable state transition at each
// stage so an interruption is always observable as a specific failed step
// rather than a silently stuck "installing" row. It intentionally lands on
// installed-disabled, never enabled: activation is a separate Enable call.
func (m *Manager) Install(ctx context.Context, req InstallRequest) (contracts.ModuleInstallation, error) {
	unlock := m.lockLifecycle()
	defer unlock()
	if req.ModuleID == "" || req.Manifest.ModuleID != req.ModuleID {
		return contracts.ModuleInstallation{}, errors.New("modules: module_id must match the manifest")
	}
	entry, ok := Lookup(req.ModuleID)
	if !ok {
		return contracts.ModuleInstallation{}, fmt.Errorf("modules: %q is not an official catalog module", req.ModuleID)
	}
	if err := req.Manifest.Validate(); err != nil {
		return contracts.ModuleInstallation{}, err
	}
	server, found, err := m.Store.GetServer(ctx, req.ServerID)
	if err != nil {
		return contracts.ModuleInstallation{}, err
	}
	if !found {
		return contracts.ModuleInstallation{}, errors.New("modules: server not found")
	}
	if server.Role != "standalone" {
		return contracts.ModuleInstallation{}, fmt.Errorf("%w: remote node installation is not configured", ErrModuleExecutorUnavailable)
	}
	if m.LocalServerID == "" || req.ServerID != m.LocalServerID {
		return contracts.ModuleInstallation{}, fmt.Errorf("%w: target is not the configured local installation", ErrModuleExecutorUnavailable)
	}
	if eligibility := CheckEligibility(server, entry, req.Manifest); !eligibility.Eligible {
		return contracts.ModuleInstallation{}, fmt.Errorf("modules: server is not eligible: %s", eligibility.Reason)
	}
	current, err := m.Store.GetModuleInstallation(ctx, req.ServerID, req.ModuleID)
	if err != nil {
		return contracts.ModuleInstallation{}, err
	}
	if current.Revision != req.ExpectedRevision {
		return contracts.ModuleInstallation{}, monitoring.ErrModuleRevisionConflict
	}

	installation, err := m.advance(ctx, req.ServerID, req.ModuleID, current, EventDownloadStart, "", req.Manifest.ModuleVersion, nil)
	if err != nil {
		return contracts.ModuleInstallation{}, err
	}
	installation, err = m.advance(ctx, req.ServerID, req.ModuleID, installation, EventVerifyStart, "", req.Manifest.ModuleVersion, nil)
	if err != nil {
		return contracts.ModuleInstallation{}, err
	}

	verifyErr := m.verifyManifest(req)
	if verifyErr != nil {
		if _, failErr := m.advance(ctx, req.ServerID, req.ModuleID, installation, EventVerifyFailed, "", req.Manifest.ModuleVersion, wireError("manifest_verification_failed", verifyErr)); failErr != nil {
			return contracts.ModuleInstallation{}, failErr
		}
		return contracts.ModuleInstallation{}, fmt.Errorf("modules: manifest verification failed: %w", verifyErr)
	}
	if policyErr := validateManifestPolicy(req.Manifest, entry, server, m.now()); policyErr != nil {
		if _, failErr := m.advance(ctx, req.ServerID, req.ModuleID, installation, EventVerifyFailed, "", req.Manifest.ModuleVersion, wireError("manifest_policy_failed", policyErr)); failErr != nil {
			return contracts.ModuleInstallation{}, failErr
		}
		return contracts.ModuleInstallation{}, policyErr
	}

	installation, err = m.advance(ctx, req.ServerID, req.ModuleID, installation, EventInstallStart, "", req.Manifest.ModuleVersion, nil)
	if err != nil {
		return contracts.ModuleInstallation{}, err
	}

	staged := m.stagingDir(req.ServerID, req.ModuleID, req.Manifest.ModuleVersion)
	active := m.installDir(req.ServerID, req.ModuleID)
	if stageErr := m.stageAndActivate(ctx, req, staged, active); stageErr != nil {
		if _, failErr := m.advance(ctx, req.ServerID, req.ModuleID, installation, EventInstallFailed, "", req.Manifest.ModuleVersion, wireError("install_failed", stageErr)); failErr != nil {
			return contracts.ModuleInstallation{}, failErr
		}
		return contracts.ModuleInstallation{}, stageErr
	}

	return m.advance(ctx, req.ServerID, req.ModuleID, installation, EventInstallOK, "", req.Manifest.ModuleVersion, nil)
}

func (m *Manager) verifyManifest(req InstallRequest) error {
	canonical, err := trust.Canonicalize(req.Manifest)
	if err != nil {
		return err
	}
	return m.Trust.Verify(req.Manifest.SigningKeyID, canonical, req.ManifestSignatureB64, m.now())
}

func (m *Manager) stageAndActivate(ctx context.Context, req InstallRequest, staged, active string) error {
	check := m.HealthCheck
	if check == nil {
		if m.Executor == nil {
			return fmt.Errorf("%w: privileged module health check is not configured", ErrModuleExecutorUnavailable)
		}
		check = func(checkCtx context.Context, moduleID, installDir string) error {
			return m.Executor.Invoke(checkCtx, ModuleInvocation{ServerID: req.ServerID, ModuleID: moduleID, ModuleVersion: req.Manifest.ModuleVersion, Operation: "health", InstallDir: installDir})
		}
	}
	if err := StageArchive(req.Archive, req.Manifest.CompressedBytes, req.Manifest.UnpackedBytes, req.Manifest.SHA256, staged, "bin/"+req.ModuleID); err != nil {
		return err
	}
	previousBackup, err := ActivateAtomic(staged, active)
	if err != nil {
		os.RemoveAll(staged)
		return err
	}
	healthCtx, cancel := context.WithTimeout(ctx, DefaultHealthCheckTimeout)
	defer cancel()
	if err := check(healthCtx, req.ModuleID, active); err != nil {
		// A failed health check is not success. Roll back to whatever was
		// active before, mirroring the release-update recovery contract.
		if removeErr := os.RemoveAll(active); removeErr != nil {
			return fmt.Errorf("modules: health check failed (%v) and failed release could not be removed: %w", err, removeErr)
		}
		if previousBackup != "" {
			if restoreErr := os.Rename(previousBackup, active); restoreErr != nil {
				return fmt.Errorf("modules: health check failed (%v) and previous release could not be restored: %w", err, restoreErr)
			}
		}
		return fmt.Errorf("modules: health check failed: %w", err)
	}
	if previousBackup != "" {
		if err := os.RemoveAll(previousBackup); err != nil {
			return fmt.Errorf("modules: installed release is healthy but previous release cleanup failed: %w", err)
		}
	}
	return nil
}

// Enable activates an installed-disabled module through the privileged
// executor, then records the durable state transition. A failed helper call
// leaves the module installed-disabled so it can be retried safely.
func (m *Manager) Enable(ctx context.Context, serverID contracts.ServerID, moduleID string, expectedRevision uint64) (contracts.ModuleInstallation, error) {
	unlock := m.lockLifecycle()
	defer unlock()
	if err := m.ensureLocalServer(ctx, serverID); err != nil {
		return contracts.ModuleInstallation{}, err
	}
	current, err := m.Store.GetModuleInstallation(ctx, serverID, moduleID)
	if err != nil {
		return contracts.ModuleInstallation{}, err
	}
	if current.Revision != expectedRevision {
		return contracts.ModuleInstallation{}, monitoring.ErrModuleRevisionConflict
	}
	if _, err := NextState(current.State, EventEnable, ""); err != nil {
		return contracts.ModuleInstallation{}, err
	}
	if m.Executor == nil {
		return contracts.ModuleInstallation{}, fmt.Errorf("%w: privileged module executor is not configured", ErrModuleExecutorUnavailable)
	}
	if err := m.Executor.Invoke(ctx, ModuleInvocation{ServerID: serverID, ModuleID: moduleID, ModuleVersion: current.Version, Operation: "enable", InstallDir: m.installDir(serverID, moduleID)}); err != nil {
		return contracts.ModuleInstallation{}, fmt.Errorf("modules: enable helper call failed: %w", err)
	}
	return m.advance(ctx, serverID, moduleID, current, EventEnable, "", current.Version, nil)
}

// Disable deactivates an enabled module back to installed-disabled. If a
// DeactivateHooks entry is registered for moduleID, it runs first and must
// succeed: "disabling ... must first revert its active policies and verify
// cleanup" (PLAN.md section 11). A hook failure leaves the module enabled
// rather than reporting a false success — the caller can retry once the
// underlying problem (e.g. a control policy that failed to revert) is fixed.
func (m *Manager) Disable(ctx context.Context, serverID contracts.ServerID, moduleID string, expectedRevision uint64) (contracts.ModuleInstallation, error) {
	unlock := m.lockLifecycle()
	defer unlock()
	if err := m.ensureLocalServer(ctx, serverID); err != nil {
		return contracts.ModuleInstallation{}, err
	}
	current, err := m.Store.GetModuleInstallation(ctx, serverID, moduleID)
	if err != nil {
		return contracts.ModuleInstallation{}, err
	}
	if current.Revision != expectedRevision {
		return contracts.ModuleInstallation{}, monitoring.ErrModuleRevisionConflict
	}
	if _, err := NextState(current.State, EventDisable, ""); err != nil {
		return contracts.ModuleInstallation{}, err
	}
	if hook, ok := m.DeactivateHooks[moduleID]; ok && hook != nil {
		if err := hook(ctx, serverID); err != nil {
			return contracts.ModuleInstallation{}, fmt.Errorf("modules: cleanup before disable failed: %w", err)
		}
	}
	if m.Executor == nil {
		return contracts.ModuleInstallation{}, fmt.Errorf("%w: privileged module executor is not configured", ErrModuleExecutorUnavailable)
	}
	if err := m.Executor.Invoke(ctx, ModuleInvocation{ServerID: serverID, ModuleID: moduleID, ModuleVersion: current.Version, Operation: "disable", InstallDir: m.installDir(serverID, moduleID)}); err != nil {
		return contracts.ModuleInstallation{}, fmt.Errorf("modules: disable helper call failed: %w", err)
	}
	return m.simpleTransition(ctx, serverID, moduleID, expectedRevision, EventDisable)
}

func (m *Manager) simpleTransition(ctx context.Context, serverID contracts.ServerID, moduleID string, expectedRevision uint64, event Event) (contracts.ModuleInstallation, error) {
	if err := m.ensureLocalServer(ctx, serverID); err != nil {
		return contracts.ModuleInstallation{}, err
	}
	current, err := m.Store.GetModuleInstallation(ctx, serverID, moduleID)
	if err != nil {
		return contracts.ModuleInstallation{}, err
	}
	if current.Revision != expectedRevision {
		return contracts.ModuleInstallation{}, monitoring.ErrModuleRevisionConflict
	}
	return m.advance(ctx, serverID, moduleID, current, event, "", current.Version, nil)
}

// Remove tears down staged/active files and returns the module to
// unavailable. It is only legal once a module is installed-disabled or
// already failed: disable first stops userspace work and reverts active
// policies, which is enforced by the lifecycle table, not by this method.
func (m *Manager) Remove(ctx context.Context, serverID contracts.ServerID, moduleID string, expectedRevision uint64) (contracts.ModuleInstallation, error) {
	unlock := m.lockLifecycle()
	defer unlock()
	if err := m.ensureLocalServer(ctx, serverID); err != nil {
		return contracts.ModuleInstallation{}, err
	}
	current, err := m.Store.GetModuleInstallation(ctx, serverID, moduleID)
	if err != nil {
		return contracts.ModuleInstallation{}, err
	}
	if current.Revision != expectedRevision {
		return contracts.ModuleInstallation{}, monitoring.ErrModuleRevisionConflict
	}
	removing, err := m.advance(ctx, serverID, moduleID, current, EventRemoveStart, "", current.Version, nil)
	if err != nil {
		return contracts.ModuleInstallation{}, err
	}
	active := m.installDir(serverID, moduleID)
	removeErr := os.RemoveAll(active)
	if removeErr == nil {
		removeErr = os.RemoveAll(active + ".previous")
	}
	if removeErr != nil {
		// Recovery executables are intentionally left in place; a failed
		// cleanup must not delete the only copy an operator could recover.
		return m.advance(ctx, serverID, moduleID, removing, EventRemoveFailed, "", current.Version, wireError("remove_failed", removeErr))
	}
	return m.advance(ctx, serverID, moduleID, removing, EventRemoveOK, "", "", nil)
}

func (m *Manager) ensureLocalServer(ctx context.Context, serverID contracts.ServerID) error {
	server, found, err := m.Store.GetServer(ctx, serverID)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("modules: server not found")
	}
	if server.Role != "standalone" {
		return fmt.Errorf("%w: remote node execution is not configured", ErrModuleExecutorUnavailable)
	}
	if m.LocalServerID == "" || serverID != m.LocalServerID {
		return fmt.Errorf("%w: target is not the configured local installation", ErrModuleExecutorUnavailable)
	}
	return nil
}

// advance validates the transition, then persists it with the store's own
// CAS so a concurrent lifecycle request for the same module cannot silently
// interleave with this one.
func (m *Manager) advance(ctx context.Context, serverID contracts.ServerID, moduleID string, current contracts.ModuleInstallation, event Event, before contracts.ModuleState, version string, moduleErr *contracts.Error) (contracts.ModuleInstallation, error) {
	next, err := NextState(current.State, event, before)
	if err != nil {
		return contracts.ModuleInstallation{}, err
	}
	result := "success"
	if next == contracts.ModuleFailed {
		result = "failure"
	}
	return m.Store.TransitionModuleInstallationAudited(ctx, serverID, moduleID, current.Revision, contracts.ModuleInstallation{
		Version: version, State: next, Error: moduleErr,
	}, "module."+string(event), result)
}

func wireError(code string, err error) *contracts.Error {
	if err == nil {
		return nil
	}
	return &contracts.Error{Code: code, Message: err.Error()}
}
