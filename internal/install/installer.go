package install

// This file contains the deliberately small, filesystem-rooted part of the
// direct installer.  It does not download or trust release artifacts.  A
// caller must provide the already verified artifacts and may inject account
// and service implementations for an SSH installer or a test.  Keeping those
// boundaries explicit makes a partial install retryable and prevents tests
// from ever touching the machine running them.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

var (
	ErrUnsupported              = errors.New("unsupported installation target")
	ErrMissingArtifact          = errors.New("required installation artifact is missing")
	ErrArtifactVerifierRequired = errors.New("artifact verification callback is required")
	ErrUnsafeArtifactPath       = errors.New("artifact path must be an absolute, clean, non-symlink path")
)

// UnsupportedError makes a failed preflight distinguishable from an
// artifact, permissions, or service failure while retaining a user-readable
// reason.  Callers should not retry this error without changing the target.
type UnsupportedError struct{ Reason string }

func (e *UnsupportedError) Error() string {
	if e == nil || strings.TrimSpace(e.Reason) == "" {
		return ErrUnsupported.Error()
	}
	return ErrUnsupported.Error() + ": " + e.Reason
}

func (e *UnsupportedError) Unwrap() error { return ErrUnsupported }

// Account is the identity used by Payesh-owned files and services.  UID/GID
// are advisory for filesystem-root fixtures; the real provisioner resolves
// them after creating the account on the live host.
type Account struct {
	Name  string
	Group string
	UID   int
	GID   int
}

// AccountManager is injectable because creating a host account is a privileged
// operation.  The default manager uses useradd only for the live root and
// writes an isolated passwd/group fixture for a non-live target root.
type AccountManager interface {
	Ensure(context.Context, string, string, string) (Account, error)
}

// ServiceManager owns enable/start/health actions.  Definitions are always
// written by Installer; this interface is only invoked when Start is true.
type ServiceManager interface {
	Apply(context.Context, string, string, []string, bool) error
}

// CommandRunner isolates external service/account commands from the installer.
// It is intentionally argument based; no command is assembled through a
// shell.
type CommandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

// InstallOptions controls a direct, role-selective installation.
type InstallOptions struct {
	Root   string
	Role   string
	Listen string

	// Artifacts maps the names returned by Preflight.Artifacts to verified
	// source files/directories.  ArtifactDir is a convenience fallback.
	Artifacts   map[string]string
	ArtifactDir string
	// InstallerPath, when set by the payesh-install command, keeps a copy of
	// the installer on the target. This makes repair and uninstall available
	// after the temporary release directory has been removed.
	InstallerPath string
	// Verify is mandatory and is called immediately before an artifact is
	// staged. A release verifier should validate the signed release manifest
	// and digest; the installer intentionally has no unsigned fallback.
	Verify func(name, path string) error

	AccountName    string
	AccountGroup   string
	AccountManager AccountManager
	ServiceManager ServiceManager
	CommandRunner  CommandRunner
	ServiceRemover ServiceRemover
	Conversion     *ConversionConfig
	Start          bool
}

// InstallResult describes work completed.  Installed is appended only after
// an atomic rename, so a caller can safely report a partial failure and retry.
type InstallResult struct {
	Preflight Preflight `json:"preflight"`
	Account   Account   `json:"account"`
	Installed []string  `json:"installed"`
	Services  []string  `json:"services"`
	Started   bool      `json:"started"`
	Resumed   bool      `json:"resumed"`
}

type installState struct {
	Role      string   `json:"role"`
	Init      string   `json:"init"`
	Installed []string `json:"installed"`
}

const (
	defaultAccount = "payesh"
	defaultGroup   = "payesh"
	dataDir        = "/var/lib/payesh"
	configDir      = "/etc/payesh"
	logDir         = "/var/log/payesh"
)

// Install performs the non-network portion of installation.  It is safe to
// call repeatedly: data and identity files are never removed, role-specific
// artifacts are the only binaries replaced, and service definitions are
// atomically rewritten.  If an operation fails, completed artifacts remain
// usable and the next invocation continues from them.
func Install(ctx context.Context, opts InstallOptions) (InstallResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	root := opts.Root
	if root == "" {
		root = "/"
	}
	role := opts.Role
	if role == "" {
		role = "node"
	}
	p, err := Check(root, role, opts.Listen)
	if err != nil {
		return InstallResult{}, err
	}
	statePath := rooted(root, filepath.Join(dataDir, "install-state.json"))
	state, stateErr := loadState(statePath)
	converting := opts.Conversion != nil
	if stateErr == nil && state.Role != role {
		if !converting || role != "node" || opts.Conversion.FromRole != state.Role {
			return InstallResult{}, fmt.Errorf("changing installed role from %s to %s requires explicit conversion", state.Role, role)
		}
	}
	if converting {
		if role != "node" {
			return InstallResult{}, errors.New("conversion target must be node")
		}
		if err := CheckHubToNode(ctx, root, *opts.Conversion); err != nil {
			return InstallResult{}, fmt.Errorf("convert hub to node: %w", err)
		}
	}
	if !p.Supported {
		return InstallResult{Preflight: p}, &UnsupportedError{Reason: strings.Join(p.Problems, "; ")}
	}
	if opts.Verify == nil {
		return InstallResult{Preflight: p}, ErrArtifactVerifierRequired
	}

	accountName := strings.TrimSpace(opts.AccountName)
	if accountName == "" {
		accountName = defaultAccount
	}
	accountGroup := strings.TrimSpace(opts.AccountGroup)
	if accountGroup == "" {
		accountGroup = defaultGroup
	}
	accountManager := opts.AccountManager
	if accountManager == nil {
		accountManager = hostAccountManager{runner: chooseRunner(opts.CommandRunner)}
	}
	account, err := accountManager.Ensure(ctx, root, accountName, accountGroup)
	if err != nil {
		return InstallResult{Preflight: p}, fmt.Errorf("ensure service account: %w", err)
	}

	result := InstallResult{Preflight: p, Account: account}
	if stateErr == nil && state.Role == role && state.Init == p.Init {
		result.Resumed = len(state.Installed) > 0
	}
	if !converting && (state.Role != role || state.Init != p.Init) {
		state = installState{Role: role, Init: p.Init}
	}

	for _, dir := range []struct {
		path string
		mode os.FileMode
	}{
		{dataDir, 0o750},
		{configDir, 0o750},
		{logDir, 0o750},
	} {
		if err := ensureDir(rooted(root, dir.path), dir.mode); err != nil {
			return result, fmt.Errorf("create %s: %w", dir.path, err)
		}
	}
	if role == "standalone" || role == "hub" {
		envPath := rooted(root, filepath.Join(configDir, "payesh.env"))
		credentialsPath := rooted(root, filepath.Join(configDir, "owner-credentials"))
		if err := ensureOwnerCredentials(envPath, credentialsPath); err != nil {
			return result, fmt.Errorf("generate owner credentials: %w", err)
		}
		if root == "/" && account.UID >= 0 && account.GID >= 0 {
			if err := os.Chown(envPath, account.UID, account.GID); err != nil {
				return result, fmt.Errorf("own owner environment: %w", err)
			}
			if err := os.Chown(credentialsPath, account.UID, account.GID); err != nil {
				return result, fmt.Errorf("own owner credentials: %w", err)
			}
		}
	}
	// A fixture root must remain entirely unprivileged from the perspective of
	// the host.  On the live root, however, the service must be able to write
	// its database, identity, spool, and logs.
	if root == "/" && account.UID >= 0 && account.GID >= 0 {
		for _, dir := range []string{dataDir, configDir, logDir} {
			if err := os.Chown(rooted(root, dir), account.UID, account.GID); err != nil {
				return result, fmt.Errorf("own %s by %s: %w", dir, account.Name, err)
			}
		}
	}

	for _, name := range p.Artifacts {
		source, err := artifactSource(opts, name)
		if err != nil {
			return result, err
		}
		if err := opts.Verify(name, source); err != nil {
			return result, fmt.Errorf("verify artifact %s: %w", name, err)
		}
		destination := artifactDestination(root, name)
		if err := installArtifact(source, destination); err != nil {
			return result, fmt.Errorf("install artifact %s: %w", name, err)
		}
		result.Installed = append(result.Installed, name)
		state.Installed = appendUnique(state.Installed, name)
		if !converting {
			if err := saveState(statePath, state); err != nil {
				return result, fmt.Errorf("save install state: %w", err)
			}
		}
	}
	if strings.TrimSpace(opts.InstallerPath) != "" {
		installerPath := strings.TrimSpace(opts.InstallerPath)
		if err := validateArtifactPath(installerPath, false); err != nil {
			return result, fmt.Errorf("validate installer artifact: %w", err)
		}
		if err := installArtifact(installerPath, artifactDestination(root, "payesh-install")); err != nil {
			return result, fmt.Errorf("install payesh-install: %w", err)
		}
		result.Installed = append(result.Installed, "payesh-install")
		state.Installed = appendUnique(state.Installed, "payesh-install")
		if !converting {
			if err := saveState(statePath, state); err != nil {
				return result, fmt.Errorf("save install state: %w", err)
			}
		}
	}
	if converting {
		remover := opts.ServiceRemover
		if remover == nil {
			remover = commandServiceRemover{runner: chooseRunner(opts.CommandRunner)}
		}
		if err := remover.Remove(ctx, root, state.Init, serviceNames(state.Role), true); err != nil {
			return result, fmt.Errorf("stop old hub services: %w", err)
		}
		if err := writeNodeConversionConfig(root, *opts.Conversion, account); err != nil {
			return result, fmt.Errorf("configure node enrollment: %w", err)
		}
		for _, path := range []string{servicePath(root, state.Init, "payesh-server"), artifactDestination(root, "payesh-server"), artifactDestination(root, "web-assets")} {
			if _, err := removeOwnedPath(path, root); err != nil {
				return result, fmt.Errorf("remove old hub artifact: %w", err)
			}
		}
		state.Role, state.Init = role, p.Init
		state.Installed = append([]string(nil), result.Installed...)
		if err := saveState(statePath, state); err != nil {
			return result, fmt.Errorf("save converted role: %w", err)
		}
	}

	services := serviceNames(role)
	result.Services = append([]string(nil), services...)
	if len(services) > 0 {
		listen := strings.TrimSpace(opts.Listen)
		if listen == "" {
			listen = DefaultWebListen
		}
		definitions := services
		if result.Resumed && opts.Listen == "" {
			definitions = nil
			for _, service := range services {
				info, err := os.Lstat(servicePath(root, p.Init, service))
				if err != nil || !info.Mode().IsRegular() {
					definitions = append(definitions, service)
				}
			}
		}
		if err := installServiceDefinitions(root, p.Init, definitions, listen); err != nil {
			return result, fmt.Errorf("install service definitions: %w", err)
		}
		if opts.Start {
			manager := opts.ServiceManager
			if manager == nil {
				manager = commandServiceManager{runner: chooseRunner(opts.CommandRunner)}
			}
			if err := manager.Apply(ctx, root, p.Init, services, true); err != nil {
				return result, fmt.Errorf("activate services: %w", err)
			}
			result.Started = true
		}
		if updateWorkerRole(role) {
			// Not fatal: the dashboard still works and reports web updates as
			// unavailable if the worker cannot be set up.
			live := opts.Start && opts.ServiceManager == nil
			if err := installUpdateWorker(ctx, root, p.Init, live, chooseRunner(opts.CommandRunner)); err != nil {
				fmt.Fprintln(os.Stderr, "warning: web update worker not installed:", err)
			}
		}
	}
	return result, nil
}

// DefaultWebListen serves the dashboard on every interface. It starts as plain
// HTTP; `payesh domain NAME` switches the same port to automatic HTTPS.
const DefaultWebListen = "0.0.0.0:8787"

func previousServerInstall(root, init string, state installState) bool {
	found := false
	for _, name := range state.Installed {
		if name == "payesh-server" {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	servicePath := "/etc/systemd/system/payesh-server.service"
	if init == "openrc" {
		servicePath = "/etc/init.d/payesh-server"
	}
	info, err := os.Lstat(rooted(root, servicePath))
	return err == nil && info.Mode().IsRegular()
}

func ensureOwnerCredentials(envPath, credentialsPath string) error {
	if _, err := os.Stat(credentialsPath); err == nil {
		return nil
	}
	// An existing environment file belongs to the operator (or to an older
	// installation). Never replace it implicitly during an upgrade; generated
	// credentials are only for a fresh configuration.
	if _, err := os.Stat(envPath); err == nil {
		return nil
	}
	randomValue := func(n int) (string, error) {
		b := make([]byte, n)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		return base64.RawURLEncoding.EncodeToString(b), nil
	}
	u, err := randomValue(9)
	if err != nil {
		return err
	}
	p, err := randomValue(24)
	if err != nil {
		return err
	}
	s, err := randomValue(24)
	if err != nil {
		return err
	}
	if err := os.WriteFile(envPath, []byte("PAYESH_OWNER_USERNAME=owner_"+u+"\nPAYESH_OWNER_PASSWORD="+p+"\nPAYESH_BOOTSTRAP_SECRET="+s+"\nPAYESH_ALLOW_INSECURE_HTTP=true\n"), 0o640); err != nil {
		return err
	}
	if err := os.WriteFile(credentialsPath, []byte("username: "+"owner_"+u+"\npassword: "+p+"\n"), 0o600); err != nil {
		return err
	}
	return nil
}

func chooseRunner(r CommandRunner) CommandRunner {
	if r != nil {
		return r
	}
	return execCommandRunner{}
}

func artifactSource(opts InstallOptions, name string) (string, error) {
	if path := strings.TrimSpace(opts.Artifacts[name]); path != "" {
		if err := validateArtifactPath(path, name == "web-assets"); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", fmt.Errorf("%w: %s (%v)", ErrMissingArtifact, name, err)
			}
			return "", fmt.Errorf("%w: %s (%v)", ErrUnsafeArtifactPath, name, err)
		}
		return path, nil
	}
	if strings.TrimSpace(opts.ArtifactDir) == "" {
		return "", fmt.Errorf("%w: %s", ErrMissingArtifact, name)
	}
	if err := validateArtifactPath(strings.TrimSpace(opts.ArtifactDir), true); err != nil {
		return "", fmt.Errorf("%w: artifact directory (%v)", ErrUnsafeArtifactPath, err)
	}
	path := filepath.Join(strings.TrimSpace(opts.ArtifactDir), name)
	if err := validateArtifactPath(path, name == "web-assets"); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %s (%v)", ErrMissingArtifact, name, err)
		}
		return "", fmt.Errorf("%w: %s (%v)", ErrUnsafeArtifactPath, name, err)
	}
	return path, nil
}

func validateArtifactPath(path string, allowDirectory bool) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) == "." || filepath.Base(path) == ".." || strings.HasPrefix(filepath.Base(path), "-") {
		return ErrUnsafeArtifactPath
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return ErrUnsafeArtifactPath
	}
	if allowDirectory {
		if !info.IsDir() {
			return fmt.Errorf("web-assets must be a directory")
		}
	} else if !info.Mode().IsRegular() {
		return fmt.Errorf("artifact must be a regular file")
	}
	if !info.IsDir() {
		return nil
	}
	return filepath.WalkDir(path, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return ErrUnsafeArtifactPath
		}
		if current != path && !entry.IsDir() && !entry.Type().IsRegular() {
			return fmt.Errorf("artifact tree contains a non-regular file")
		}
		return nil
	})
}

func artifactDestination(root, name string) string {
	if name == "web-assets" {
		return rooted(root, "/usr/share/payesh/web-assets")
	}
	return rooted(root, "/usr/bin/"+name)
}

func rooted(root, absolute string) string {
	if root == "/" {
		return filepath.Clean(absolute)
	}
	return filepath.Join(root, strings.TrimPrefix(filepath.Clean(absolute), string(filepath.Separator)))
}

func ensureDir(path string, mode os.FileMode) error {
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func installArtifact(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return ErrUnsafeArtifactPath
	}
	if info.IsDir() && filepath.Base(destination) != "web-assets" {
		return fmt.Errorf("artifact source is a directory")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(destination), ".payesh-stage-")
	if err != nil {
		return err
	}
	tmpPath := filepath.Join(tmp, filepath.Base(destination))
	defer os.RemoveAll(tmp)
	if info.IsDir() {
		if err := copyTree(source, tmpPath); err != nil {
			return err
		}
		if err := os.Chmod(tmpPath, 0o755); err != nil {
			return err
		}
	} else {
		if err := copyFile(source, tmpPath, 0o755); err != nil {
			return err
		}
	}
	if !info.IsDir() {
		return os.Rename(tmpPath, destination)
	}
	// A rename cannot replace a non-empty directory. Preserve the prior web
	// bundle until the staged bundle is active so repeated installs work and a
	// failed activation can restore the last complete bundle.
	previous := destination + ".previous"
	if err := os.RemoveAll(previous); err != nil {
		return fmt.Errorf("remove stale artifact backup: %w", err)
	}
	hadPrevious := false
	if _, err := os.Stat(destination); err == nil {
		if err := os.Rename(destination, previous); err != nil {
			return fmt.Errorf("preserve previous artifact: %w", err)
		}
		hadPrevious = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(tmpPath, destination); err != nil {
		if hadPrevious {
			if restoreErr := os.Rename(previous, destination); restoreErr != nil {
				return fmt.Errorf("activate artifact: %v; restore previous artifact: %w", err, restoreErr)
			}
		}
		return fmt.Errorf("activate artifact: %w", err)
	}
	if hadPrevious {
		if err := os.RemoveAll(previous); err != nil {
			return fmt.Errorf("remove previous artifact after activation: %w", err)
		}
	}
	return nil
}

func copyFile(source, destination string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err = out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func copyTree(source, destination string) error {
	return filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported non-regular web asset %s", rel)
		}
		return copyFile(path, target, 0o644)
	})
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func loadState(path string) (installState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return installState{}, err
	}
	var state installState
	if err := json.Unmarshal(b, &state); err != nil {
		return installState{}, err
	}
	return state, nil
}

func saveState(path string, state installState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".install-state-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func serviceNames(role string) []string {
	switch role {
	case "standalone", "hub":
		return []string{"payesh-agent", "payesh-server"}
	case "node":
		return []string{"payesh-agent"}
	default:
		return nil
	}
}

func installServiceDefinitions(root, init string, services []string, listen string) error {
	for _, service := range services {
		content, ok := serviceDefinition(init, service, listen)
		if !ok {
			return &UnsupportedError{Reason: "no service definition for " + init + "/" + service}
		}
		path := servicePath(root, init, service)
		if err := writeAtomic(path, []byte(content), serviceMode(init)); err != nil {
			return fmt.Errorf("%s: %w", service, err)
		}
	}
	return nil
}

func servicePath(root, init, service string) string {
	if init == "openrc" {
		return rooted(root, "/etc/init.d/"+service)
	}
	return rooted(root, "/etc/systemd/system/"+service+".service")
}

func serviceMode(init string) os.FileMode {
	if init == "openrc" {
		return 0o755
	}
	return 0o644
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".payesh-service-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Service definitions are kept here as a release fallback.  The deploy/
// copies remain the human-editable source and are intentionally equivalent.
func serviceDefinition(init, service, listen string) (string, bool) {
	if init == "systemd" {
		switch service {
		case "payesh-agent":
			return "[Unit]\nDescription=Payesh local monitoring agent\nAfter=network-online.target\nWants=network-online.target\n\n[Service]\nType=simple\nUser=payesh\nGroup=payesh\nEnvironmentFile=-/etc/payesh/payesh.env\nExecStartPre=/bin/sh -c 'if [ -z \"$PAYESH_TRANSPORT_URL\" ]; then /usr/bin/payesh register-server -ensure -db=/var/lib/payesh/payesh.db -identity-file=/var/lib/payesh/server-id >/dev/null; fi'\nExecStart=/bin/bash -o pipefail -c 'if [ -n \"$PAYESH_TRANSPORT_URL\" ]; then exec /usr/bin/payesh-agent -interval=15s; else exec /usr/bin/payesh-agent -interval=15s -identity-file=/var/lib/payesh/server-id | /usr/bin/payesh ingest -db=/var/lib/payesh/payesh.db -identity-file=/var/lib/payesh/server-id -follow >/dev/null; fi'\nRestart=on-failure\nRestartSec=5s\nKillMode=control-group\nNoNewPrivileges=yes\nPrivateTmp=yes\nProtectHome=yes\nProtectSystem=strict\nReadWritePaths=/var/lib/payesh /var/log/payesh\nRestrictAddressFamilies=AF_UNIX AF_INET AF_INET6\nCapabilityBoundingSet=\nAmbientCapabilities=\nStandardOutput=null\nStandardError=append:/var/log/payesh/agent.err\n\n[Install]\nWantedBy=multi-user.target\n", true
		case "payesh-server":
			return fmt.Sprintf("[Unit]\nDescription=Payesh local monitoring server\nAfter=network-online.target\nWants=network-online.target\n\n[Service]\nType=simple\nUser=payesh\nGroup=payesh\nEnvironmentFile=-/etc/payesh/payesh.env\nExecStart=/usr/bin/payesh-server -db=/var/lib/payesh/payesh.db -listen=%s\nStandardOutput=append:/var/log/payesh/server.err\nStandardError=append:/var/log/payesh/server.err\nRestart=on-failure\nRestartSec=5s\nAmbientCapabilities=CAP_NET_BIND_SERVICE\nCapabilityBoundingSet=CAP_NET_BIND_SERVICE\nNoNewPrivileges=yes\nPrivateTmp=yes\nProtectHome=yes\nProtectSystem=strict\nReadWritePaths=/var/lib/payesh /var/log/payesh\n\n[Install]\nWantedBy=multi-user.target\n", systemdArg(listen)), true
		}
	}
	if init == "openrc" {
		switch service {
		case "payesh-agent":
			return "#!/sbin/openrc-run\nname=\"payesh-agent\"\ndescription=\"Payesh local monitoring agent\"\ncommand=\"/bin/sh\"\ncommand_args=\"-c 'set -o pipefail; if [ -f /etc/payesh/payesh.env ]; then . /etc/payesh/payesh.env; fi; if [ -n \\\"$PAYESH_TRANSPORT_URL\\\" ]; then exec /usr/bin/payesh-agent -interval=15s; else exec /usr/bin/payesh-agent -interval=15s -identity-file=/var/lib/payesh/server-id | /usr/bin/payesh ingest -db=/var/lib/payesh/payesh.db -identity-file=/var/lib/payesh/server-id -follow >/dev/null; fi'\"\ncommand_user=\"payesh:payesh\"\nsupervisor=\"supervise-daemon\"\nsupervise_daemon_args=\"--respawn-delay 5\"\noutput_log=\"/dev/null\"\nerror_log=\"/var/log/payesh/agent.err\"\n\nstart_pre() {\n\tif [ -z \"$PAYESH_TRANSPORT_URL\" ]; then /usr/bin/payesh register-server -ensure -db=/var/lib/payesh/payesh.db -identity-file=/var/lib/payesh/server-id >/dev/null || return 1; fi\n}\n\ndepend() {\n\tneed net\n\tafter firewall\n}\n", true
		case "payesh-server":
			return fmt.Sprintf("#!/sbin/openrc-run\nname=\"payesh-server\"\ndescription=\"Payesh local monitoring server\"\ncommand=\"/usr/bin/payesh-server\"\ncommand_args=\"-db=/var/lib/payesh/payesh.db -listen=%s\"\ncommand_user=\"payesh:payesh\"\ncapabilities=\"^cap_net_bind_service\"\nsupervisor=\"supervise-daemon\"\nsupervise_daemon_args=\"--respawn-delay 5\"\noutput_log=\"/var/log/payesh/server.log\"\nerror_log=\"/var/log/payesh/server.err\"\n\ndepend() {\n\tneed net\n\tafter firewall\n}\n", openRCArg(listen)), true
		}
	}
	return "", false
}

// systemd treats '%' as a unit-specifier introducer even inside ExecStart.
// Keep the address literal while preventing an operator-supplied '%' from
// being interpreted by the service manager.
func systemdArg(value string) string { return strings.ReplaceAll(value, "%", "%%") }

// OpenRC's command_args is parsed as a shell-like assignment. Check() rejects
// whitespace/quotes in listen addresses, so this escaping is defensive and
// keeps the generated definition safe if called directly in a future test.
func openRCArg(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return value
}

type commandServiceManager struct{ runner CommandRunner }

func (m commandServiceManager) Apply(ctx context.Context, root, init string, services []string, start bool) error {
	if root != "/" {
		return &UnsupportedError{Reason: "starting services requires the live root (use an injected service manager for a fixture root)"}
	}
	if init == "systemd" {
		args := []string{"daemon-reload"}
		if out, err := m.runner.Run(ctx, "systemctl", args...); err != nil {
			return commandError("systemctl daemon-reload", out, err)
		}
		for _, service := range services {
			args := []string{"enable", service}
			if out, err := m.runner.Run(ctx, "systemctl", args...); err != nil {
				return commandError("systemctl enable "+service, out, err)
			}
			if start {
				if out, err := m.runner.Run(ctx, "systemctl", "restart", service); err != nil {
					return commandError("systemctl restart "+service, out, err)
				}
				if out, err := m.runner.Run(ctx, "systemctl", "is-active", "--quiet", service); err != nil {
					return commandError("systemctl health "+service, out, err)
				}
			}
		}
		return nil
	}
	if init != "openrc" {
		return &UnsupportedError{Reason: "service manager " + init}
	}
	for _, service := range services {
		if out, err := m.runner.Run(ctx, "rc-update", "add", service, "default"); err != nil {
			return commandError("rc-update add "+service, out, err)
		}
		if start {
			if out, err := m.runner.Run(ctx, "rc-service", service, "restart"); err != nil {
				return commandError("rc-service restart "+service, out, err)
			}
			if out, err := m.runner.Run(ctx, "rc-service", service, "status"); err != nil {
				return commandError("rc-service health "+service, out, err)
			}
		}
	}
	return nil
}

func commandError(operation string, output []byte, err error) error {
	detail := strings.TrimSpace(string(output))
	if len(detail) > 512 {
		detail = detail[:512]
	}
	if detail == "" {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return fmt.Errorf("%s: %w (%s)", operation, err, detail)
}

type hostAccountManager struct{ runner CommandRunner }

func (m hostAccountManager) Ensure(ctx context.Context, root, name, group string) (Account, error) {
	if root != "/" {
		return ensureFixtureAccount(root, name, group)
	}
	uid, gid, err := lookupAccount(name, group)
	if err != nil {
		if out, runErr := m.runner.Run(ctx, "useradd", "--system", "--home-dir", dataDir, "--shell", "/usr/sbin/nologin", "--user-group", name); runErr != nil {
			return Account{}, commandError("useradd "+name, out, runErr)
		}
		uid, gid, err = lookupAccount(name, group)
		if err != nil {
			return Account{}, fmt.Errorf("resolve service account after useradd: %w", err)
		}
	}
	return Account{Name: name, Group: group, UID: uid, GID: gid}, nil
}

func lookupAccount(name, group string) (int, int, error) {
	out, err := exec.Command("id", "-u", name).Output()
	if err != nil {
		return 0, 0, err
	}
	uid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, 0, err
	}
	out, err = exec.Command("id", "-g", group).Output()
	if err != nil {
		return 0, 0, err
	}
	gid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

func ensureFixtureAccount(root, name, group string) (Account, error) {
	passwd := rooted(root, "/etc/passwd")
	groups := rooted(root, "/etc/group")
	if err := os.MkdirAll(filepath.Dir(passwd), 0o755); err != nil {
		return Account{}, err
	}
	uid, gid := 997, 997
	if b, err := os.ReadFile(passwd); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			fields := strings.Split(line, ":")
			if len(fields) > 3 && fields[0] == name {
				uid, _ = strconv.Atoi(fields[2])
			}
		}
	}
	if b, err := os.ReadFile(groups); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			fields := strings.Split(line, ":")
			if len(fields) > 2 && fields[0] == group {
				gid, _ = strconv.Atoi(fields[2])
			}
		}
	}
	if _, err := os.Stat(passwd); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(passwd, []byte("root:x:0:0:root:/root:/bin/sh\n"), 0o644); err != nil {
			return Account{}, err
		}
	}
	if _, err := os.Stat(groups); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(groups, []byte("root:x:0:\n"), 0o644); err != nil {
			return Account{}, err
		}
	}
	// Existing entries are retained verbatim; this makes retries idempotent.
	b, _ := os.ReadFile(passwd)
	if !containsAccountLine(string(b), name) {
		f, err := os.OpenFile(passwd, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return Account{}, err
		}
		_, err = fmt.Fprintf(f, "%s:x:%d:%d:Payesh:/var/lib/payesh:/usr/sbin/nologin\n", name, uid, gid)
		_ = f.Close()
		if err != nil {
			return Account{}, err
		}
	}
	b, _ = os.ReadFile(groups)
	if !containsAccountLine(string(b), group) {
		f, err := os.OpenFile(groups, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return Account{}, err
		}
		_, err = fmt.Fprintf(f, "%s:x:%d:\n", group, gid)
		_ = f.Close()
		if err != nil {
			return Account{}, err
		}
	}
	return Account{Name: name, Group: group, UID: uid, GID: gid}, nil
}

func containsAccountLine(contents, name string) bool {
	for _, line := range strings.Split(contents, "\n") {
		if strings.HasPrefix(line, name+":") {
			return true
		}
	}
	return false
}
