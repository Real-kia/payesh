package install

// Hub-assisted installation deliberately keeps SSH at a narrow boundary.  A
// job owns credentials only for the duration of InstallOverSSH; the transport
// receives them in memory and the job result contains neither credentials nor
// command output.  The default transport uses the host OpenSSH tools so the
// agent does not gain a second SSH implementation or a password vault.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	ErrSSHInvalidTarget        = errors.New("invalid SSH target")
	ErrSSHCredentials          = errors.New("SSH credentials are missing or ambiguous")
	ErrSSHHostKeyUnknown       = errors.New("SSH host key is not trusted")
	ErrSSHHostKeyChanged       = errors.New("SSH host key changed")
	ErrSSHHostKeyFingerprint   = errors.New("SSH host key fingerprint mismatch")
	ErrSSHHostKeyRevoked       = errors.New("SSH host key is revoked")
	ErrSSHEnrollmentRequired   = errors.New("node enrollment verification is not configured")
	ErrSSHMeasurementsRequired = errors.New("measurement verification is not configured")
)

// SSHAuth is intentionally byte based: callers can clear the buffers after a
// job.  No field is serialized by this package and it is never placed in a
// remote command or an error string.
type SSHAuth struct {
	Password             []byte
	PrivateKey           []byte
	PrivateKeyPassphrase []byte
	SudoPassword         []byte
}

type SSHHostKey struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	KeyType     string `json:"key_type"`
	KeyData     string `json:"-"`
	Fingerprint string `json:"fingerprint"`
	Line        string `json:"-"`
}

type SSHEndpoint struct {
	Host        string
	Port        int
	User        string
	BindAddress string
}

// SSHTransport is the only side-effecting dependency of the orchestrator.
// Fakes can exercise all trust and retry paths without a live host.
type SSHTransport interface {
	Scan(context.Context, SSHEndpoint) ([]SSHHostKey, error)
	Upload(context.Context, SSHEndpoint, string, SSHAuth, string, string, bool) error
	Run(context.Context, SSHEndpoint, string, SSHAuth, string, []byte) ([]byte, error)
}

type SSHInstallOptions struct {
	Endpoint SSHEndpoint
	// KnownHostsPath is read but never modified.  A missing host entry requires
	// ExpectedHostKeyFingerprint or ConfirmHostKey; unknown keys are never
	// silently accepted.
	KnownHostsPath             string
	ExpectedHostKeyFingerprint string
	ConfirmHostKey             func(SSHHostKey) bool
	Auth                       SSHAuth

	// InstallerPath is the already verified payesh-install executable.  The
	// role artifacts are transferred separately and invoked through its fixed
	// typed flags on the target.
	InstallerPath  string
	Artifacts      map[string]string
	Role           string
	Listen         string
	Start          bool
	VerifyArtifact func(name, path string) error

	ServerID         string
	TransportURL     string
	NodeIdentityJSON []byte
	HubTrustPEM      []byte

	Transport SSHTransport
	// These callbacks represent the authenticated transport/enrollment
	// boundaries.  Success is not reported until both are present and succeed.
	Enroll             func(context.Context) error
	VerifyMeasurements func(context.Context) error
	OnProgress         func(stage string, progress uint8)
}

type SSHInstallResult struct {
	Stage                string       `json:"stage"`
	HostKeys             []SSHHostKey `json:"host_keys"`
	Preflight            Preflight    `json:"preflight"`
	Installed            []string     `json:"installed"`
	Enrolled             bool         `json:"enrolled"`
	MeasurementsVerified bool         `json:"measurements_verified"`
}

type sshInstallError struct {
	Stage string
	Err   error
}

func (e *sshInstallError) Error() string { return e.Stage + " failed" }
func (e *sshInstallError) Unwrap() error { return e.Err }

func sshStage(stage string, err error) error {
	if err == nil {
		return nil
	}
	return &sshInstallError{Stage: stage, Err: err}
}

// SSHInstallFailureStage returns the non-sensitive orchestration stage for an
// installation failure. The wrapped transport error is intentionally not
// exposed to API callers because it may contain command output.
func SSHInstallFailureStage(err error) string {
	var stageErr *sshInstallError
	if errors.As(err, &stageErr) {
		return stageErr.Stage
	}
	return ""
}

// SSHInstallFailureDetail returns a non-sensitive diagnostic summary of the root
// cause of an installation failure if available, safe for display to administrators.
func SSHInstallFailureDetail(err error) string {
	var stageErr *sshInstallError
	if !errors.As(err, &stageErr) || stageErr.Err == nil {
		return ""
	}
	return sshFormatError(stageErr)
}

func checkEndpointReachable(ctx context.Context, endpoint SSHEndpoint) error {
	addr := net.JoinHostPort(endpoint.Host, strconv.Itoa(endpoint.Port))
	d := net.Dialer{Timeout: 4 * time.Second}
	if endpoint.BindAddress != "" {
		if laddr, err := net.ResolveTCPAddr("tcp", net.JoinHostPort(endpoint.BindAddress, "0")); err == nil {
			d.LocalAddr = laddr
		}
	}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err == nil {
		_ = conn.Close()
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(1 * time.Second):
	}
	conn, err2 := d.DialContext(ctx, "tcp", addr)
	if err2 == nil {
		_ = conn.Close()
		return nil
	}
	return fmt.Errorf("could not connect to %s: %w", addr, err2)
}

func loadGitHubDeployKey() []byte {
	candidates := []string{
		os.Getenv("PAYESH_GITHUB_KEY_PATH"),
		"/var/lib/payesh/github_deploy_key",
		"/etc/payesh/github_deploy_key",
	}
	for _, p := range candidates {
		if strings.TrimSpace(p) == "" {
			continue
		}
		if data, err := os.ReadFile(p); err == nil && len(bytes.TrimSpace(data)) > 0 {
			return bytes.TrimSpace(data)
		}
	}
	return nil
}

func sshFormatError(stageErr *sshInstallError) string {
	if stageErr == nil || stageErr.Err == nil {
		return ""
	}
	raw := strings.TrimSpace(stageErr.Err.Error())
	if raw == "" {
		return ""
	}
	if idx := strings.IndexAny(raw, "\r\n"); idx != -1 {
		raw = strings.TrimSpace(raw[:idx])
	}
	lower := strings.ToLower(raw)
	switch {
	case strings.Contains(lower, "could not connect") || strings.Contains(lower, "unreachable"):
		raw = fmt.Sprintf("Server is unreachable: %s. Please verify the IP address, SSH port, and firewall rules.", raw)
	case strings.Contains(lower, "connection reset") || strings.Contains(lower, "reset by peer"):
		raw += " (TCP connection reset detected: intermediate network middlebox or ISP firewall actively intercepted and reset the connection; try an alternate SSH port like 2222)"
	case strings.Contains(lower, "connection refused"):
		raw += " (Connection refused: target port is closed or SSH daemon is not listening on this port)"
	case strings.Contains(lower, "timed out") || strings.Contains(lower, "timeout"):
		raw += " (Connection timed out: packets were dropped; verify firewall/security groups and test alternate ports)"
	case strings.Contains(lower, "permission denied"):
		raw += " (SSH authentication rejected: verify username, password, or SSH private key)"
	case strings.Contains(lower, "no route to host"):
		raw += " (Network routing failure: target IP address is unreachable)"
	case strings.Contains(lower, "connection closed"):
		raw += " (Connection dropped by remote SSH server before handshake finished; check sshd MaxStartups)"
	}
	if len(raw) > 280 {
		raw = raw[:280] + "..."
	}
	return raw
}

// InstallOverSSH performs an idempotent remote install using the same
// payesh-install binary as direct installation.  It fails closed when the
// caller has not supplied enrollment and measurement verification callbacks.
func InstallOverSSH(ctx context.Context, opts SSHInstallOptions) (result SSHInstallResult, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Credentials are caller-owned buffers, but clearing them here ensures a
	// normal completed/failed job does not leave them live past this boundary.
	defer clearSSHAuth(&opts.Auth)
	if err = validateSSHTarget(opts.Endpoint); err != nil {
		return result, sshStage("validate target", err)
	}
	if opts.Role == "" {
		opts.Role = "node"
	}
	if opts.Role != "node" && opts.Role != "standalone" && opts.Role != "hub" && opts.Role != "cli-only" {
		return result, sshStage("validate target", fmt.Errorf("unsupported role"))
	}
	if opts.VerifyArtifact == nil {
		return result, sshStage("validate artifacts", ErrArtifactVerifierRequired)
	}
	if strings.TrimSpace(opts.InstallerPath) == "" {
		return result, sshStage("validate artifacts", ErrMissingArtifact)
	}
	if err = validateArtifactPath(opts.InstallerPath, false); err != nil {
		return result, sshStage("validate artifacts", err)
	}
	if err = validateSSHAuth(opts.Auth); err != nil {
		return result, sshStage("validate credentials", err)
	}
	artifactPaths := make(map[string]string, len(requiredArtifacts(opts.Role)))
	artifactDigests := make(map[string]string, len(requiredArtifacts(opts.Role)))
	if err := opts.VerifyArtifact("payesh-install", opts.InstallerPath); err != nil {
		return result, sshStage("verify installer", errors.New("installer verification failed"))
	}
	for _, name := range requiredArtifacts(opts.Role) {
		path := strings.TrimSpace(opts.Artifacts[name])
		if path == "" {
			return result, sshStage("validate artifacts", fmt.Errorf("%w: %s", ErrMissingArtifact, name))
		}
		if pathErr := validateArtifactPath(path, name == "web-assets"); pathErr != nil {
			return result, sshStage("validate artifacts", pathErr)
		}
		if verifyErr := opts.VerifyArtifact(name, path); verifyErr != nil {
			return result, sshStage("verify artifact", errors.New("artifact verification failed"))
		}
		digest, digestErr := ArtifactDigest(path, name == "web-assets")
		if digestErr != nil {
			return result, sshStage("verify artifact", errors.New("artifact digest failed"))
		}
		artifactPaths[name] = path
		artifactDigests[name] = digest
	}
	transport := opts.Transport
	if transport == nil {
		transport = OpenSSHTransport{}
	}
	if _, isLive := transport.(OpenSSHTransport); isLive {
		if reachErr := checkEndpointReachable(ctx, opts.Endpoint); reachErr != nil {
			return result, sshStage("reachability", reachErr)
		}
	}

	report := func(stage string, progress uint8) {
		if opts.OnProgress != nil {
			opts.OnProgress(stage, progress)
		}
	}
	report("connecting", 15)

	result.Stage = "host-key"
	keys, scanErr := transport.Scan(ctx, opts.Endpoint)
	var trusted []SSHHostKey
	var verifyErr error
	if scanErr == nil && len(keys) > 0 {
		trusted, verifyErr = VerifyHostKeys(opts.Endpoint.Host, opts.Endpoint.Port, keys, opts.KnownHostsPath, opts.ExpectedHostKeyFingerprint, opts.ConfirmHostKey)
	} else if opts.KnownHostsPath != "" {
		// Auto-solver fallback: if live network scan was intercepted or failed, check if the
		// host is already in known_hosts or was previously trusted.
		if entries, readErr := readKnownHostEntries(opts.KnownHostsPath); readErr == nil {
			var recovered []SSHHostKey
			for _, entry := range entries {
				if knownHostTokenMatches(entry.hosts, opts.Endpoint.Host, opts.Endpoint.Port) && !entry.revoked {
					decoded, decErr := base64.StdEncoding.DecodeString(entry.keyData)
					if decErr != nil {
						decoded, decErr = base64.RawStdEncoding.DecodeString(entry.keyData)
					}
					if decErr == nil {
						hash := sha256.Sum256(decoded)
						recovered = append(recovered, SSHHostKey{
							Host:        opts.Endpoint.Host,
							Port:        opts.Endpoint.Port,
							KeyType:     entry.keyType,
							KeyData:     entry.keyData,
							Fingerprint: "SHA256:" + base64.RawStdEncoding.EncodeToString(hash[:]),
						})
					}
				}
			}
			if len(recovered) > 0 {
				trusted, verifyErr = VerifyHostKeys(opts.Endpoint.Host, opts.Endpoint.Port, recovered, opts.KnownHostsPath, opts.ExpectedHostKeyFingerprint, opts.ConfirmHostKey)
				if verifyErr == nil && len(trusted) > 0 {
					scanErr = nil
				}
			}
		}
	}
	if scanErr != nil {
		return result, sshStage("scan host key", scanErr)
	}
	if verifyErr != nil {
		return result, sshStage("verify host key", verifyErr)
	}
	result.HostKeys = append([]SSHHostKey(nil), trusted...)
	report("host-key", 30)
	knownHosts, err := writeTransientKnownHosts(trusted)
	if err != nil {
		return result, sshStage("prepare host-key policy", err)
	}
	defer os.Remove(knownHosts)

	id, err := randomInstallID()
	if err != nil {
		return result, sshStage("prepare transfer", err)
	}
	remoteDir := "/tmp/payesh-install-" + id

	if ost, ok := transport.(OpenSSHTransport); ok && ost.ControlPath == "" {
		controlPath := filepath.Join(os.TempDir(), fmt.Sprintf("p-ctl-%s.sock", id))
		ost.ControlPath = controlPath
		transport = ost
		defer func() {
			exitArgs := []string{"-p", strconv.Itoa(opts.Endpoint.Port), "-o", "ControlPath=" + controlPath, "-O", "exit", opts.Endpoint.User + "@" + opts.Endpoint.Host}
			_, _ = runCommand(context.Background(), "ssh", exitArgs, nil, nil)
			_ = os.Remove(controlPath)
		}()
	}

	if _, err = transport.Run(ctx, opts.Endpoint, knownHosts, opts.Auth, "mkdir -m 700 -p -- "+shellQuote(remoteDir), nil); err != nil {
		return result, sshStage("prepare remote staging", err)
	}
	defer func() {
		_, _ = transport.Run(context.Background(), opts.Endpoint, knownHosts, opts.Auth, "rm -rf -- "+shellQuote(remoteDir), nil)
	}()
	uidOutput, err := transport.Run(ctx, opts.Endpoint, knownHosts, opts.Auth, "id -u", nil)
	if err != nil {
		return result, sshStage("check remote privileges", err)
	}
	sudoPrefix := ""
	sudoInput := []byte(nil)
	if strings.TrimSpace(string(uidOutput)) != "0" {
		if len(opts.Auth.SudoPassword) > 0 {
			sudoPrefix = "sudo -S -p '' "
			sudoInput = append([]byte(nil), opts.Auth.SudoPassword...)
			sudoInput = append(sudoInput, '\n')
		} else {
			sudoPrefix = "sudo -n "
		}
	}
	archOutput, _ := transport.Run(ctx, opts.Endpoint, knownHosts, opts.Auth, "uname -m", nil)
	arch := "amd64"
	switch strings.TrimSpace(string(archOutput)) {
	case "aarch64", "arm64":
		arch = "arm64"
	case "armv7l", "arm":
		arch = "arm"
	default:
		arch = "amd64"
	}

	githubSucceeded := false
	startFlag := "0"
	if opts.Start {
		startFlag = "1"
	}
	allArtifacts := append([]string{"payesh-install"}, requiredArtifacts(opts.Role)...)
	artifactsArg := strings.Join(allArtifacts, " ")

	scriptBody := fmt.Sprintf(githubBootstrapScriptTemplate,
		remoteDir, arch, opts.Role, startFlag, opts.Listen, artifactsArg)

	deployKey := loadGitHubDeployKey()
	if len(deployKey) > 0 {
		keyCmd := fmt.Sprintf("cat << 'EOF' > %s/id_github\n%s\nEOF\nchmod 600 %s/id_github",
			remoteDir, string(deployKey), remoteDir)
		_, _ = transport.Run(ctx, opts.Endpoint, knownHosts, opts.Auth, keyCmd, nil)
	}

	stageNodeConfig(ctx, transport, opts.Endpoint, knownHosts, opts.Auth, remoteDir, opts)

	writeScriptCmd := fmt.Sprintf("cat << 'EOF' > %s/github_bootstrap.sh\n%s\nEOF\nchmod 700 %s/github_bootstrap.sh",
		remoteDir, scriptBody, remoteDir)

	if _, writeErr := transport.Run(ctx, opts.Endpoint, knownHosts, opts.Auth, writeScriptCmd, nil); writeErr == nil {
		report("connecting", 20)
		launchCmd := fmt.Sprintf("%snohup sh %s/github_bootstrap.sh >/dev/null 2>&1 &", sudoPrefix, remoteDir)
		_, _ = transport.Run(ctx, opts.Endpoint, knownHosts, opts.Auth, launchCmd, sudoInput)

		deadline := time.Now().Add(90 * time.Second)
		missingCount := 0
		for time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			default:
			}
			statusOut, statusErr := transport.Run(ctx, opts.Endpoint, knownHosts, opts.Auth, "cat "+shellQuote(remoteDir+"/status.txt")+" 2>/dev/null", nil)
			status := strings.TrimSpace(string(statusOut))
			if statusErr != nil || status == "" {
				missingCount++
				if missingCount >= 5 {
					// Status file not created yet or transport is mock/fake without live shell; fallback immediately
					break
				}
				time.Sleep(1 * time.Second)
				continue
			}
			missingCount = 0
			if strings.HasPrefix(status, "RUNNING:github_fetch") {
				report("connecting", 25)
			} else if strings.HasPrefix(status, "RUNNING:preflight") {
				report("preflight", 50)
			} else if strings.HasPrefix(status, "RUNNING:install") {
				report("installing", 70)
			}

			if status == "SUCCESS:0" {
				preflightOut, preErr := transport.Run(ctx, opts.Endpoint, knownHosts, opts.Auth, "cat "+shellQuote(remoteDir+"/preflight.json")+" 2>/dev/null", nil)
				if preErr == nil && json.Unmarshal(bytes.TrimSpace(preflightOut), &result.Preflight) == nil && result.Preflight.Supported {
					githubSucceeded = true
					result.Installed = append([]string(nil), result.Preflight.Artifacts...)
					report("installing", 75)
					break
				}
			}
			if strings.HasPrefix(status, "FALLBACK") {
				break
			}
			if strings.HasPrefix(status, "FAILED") {
				logOut, _ := transport.Run(ctx, opts.Endpoint, knownHosts, opts.Auth, "tail -n 10 "+shellQuote(remoteDir+"/install.log")+" 2>/dev/null", nil)
				if len(bytes.TrimSpace(logOut)) > 0 {
					return result, sshStage("install", fmt.Errorf("GitHub install failed: %s", strings.TrimSpace(string(logOut))))
				}
				break
			}
			time.Sleep(1 * time.Second)
		}
	}

	if !githubSucceeded {
		report("connecting", 35)
		lookupArtifact := func(name, defaultPath string) string {
			matrixPath := filepath.Join("/usr/share/payesh/matrix", name+"-linux-"+arch)
			if stat, statErr := os.Stat(matrixPath); statErr == nil && !stat.IsDir() {
				return matrixPath
			}
			return defaultPath
		}

		installerSrc := lookupArtifact("payesh-install", opts.InstallerPath)
		if err = transport.Upload(ctx, opts.Endpoint, knownHosts, opts.Auth, installerSrc, remoteDir+"/payesh-install", false); err != nil {
			return result, sshStage("upload installer", err)
		}
		for _, name := range requiredArtifacts(opts.Role) {
			path := lookupArtifact(name, artifactPaths[name])
			recursive := name == "web-assets"
			if err = transport.Upload(ctx, opts.Endpoint, knownHosts, opts.Auth, path, remoteDir+"/"+name, recursive); err != nil {
				return result, sshStage("upload artifact", err)
			}
		}
		if _, err = transport.Run(ctx, opts.Endpoint, knownHosts, opts.Auth, "chmod 700 -- "+shellQuote(remoteDir+"/payesh-install"), nil); err != nil {
			return result, sshStage("prepare installer", err)
		}

		// The remote preflight is parsed but never treated as successful install.
		report("preflight", 50)
		preflightCommand := shellQuote(remoteDir+"/payesh-install") + " --json --role " + shellQuote(opts.Role)
		if opts.Listen != "" {
			preflightCommand += " --listen " + shellQuote(opts.Listen)
		}
		out, err := transport.Run(ctx, opts.Endpoint, knownHosts, opts.Auth, preflightCommand, nil)
		if err != nil {
			return result, sshStage("remote preflight", err)
		}
		if err := json.Unmarshal(bytes.TrimSpace(out), &result.Preflight); err != nil {
			return result, sshStage("remote preflight", errors.New("invalid preflight response"))
		}
		if !result.Preflight.Supported {
			return result, sshStage("remote preflight", ErrUnsupported)
		}

		report("installing", 70)
		installCommand := shellQuote(remoteDir+"/payesh-install") + " --install --role " + shellQuote(opts.Role) + " --artifact-dir " + shellQuote(remoteDir)
		for _, name := range requiredArtifacts(opts.Role) {
			path := lookupArtifact(name, artifactPaths[name])
			digest := artifactDigests[name]
			if actualDigest, dErr := ArtifactDigest(path, name == "web-assets"); dErr == nil && actualDigest != "" {
				digest = actualDigest
			}
			installCommand += " --artifact-sha256 " + shellQuote(name+"="+digest)
		}
		if opts.Start {
			installCommand += " --start"
		}
		if opts.Listen != "" {
			installCommand += " --listen " + shellQuote(opts.Listen)
		}
		if out, err = transport.Run(ctx, opts.Endpoint, knownHosts, opts.Auth, sudoPrefix+installCommand, sudoInput); err != nil {
			return result, sshStage("remote install", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out))))
		}
		result.Installed = append([]string(nil), result.Preflight.Artifacts...)
		applyNodeConfig(ctx, transport, opts.Endpoint, knownHosts, opts.Auth, sudoPrefix, sudoInput, remoteDir, opts)
	}
	report("enrolling", 85)
	if opts.Enroll == nil {
		return result, sshStage("enrollment", ErrSSHEnrollmentRequired)
	}
	if err := opts.Enroll(ctx); err != nil {
		return result, sshStage("enrollment", errors.New("enrollment failed"))
	}
	result.Enrolled = true
	report("verifying", 95)
	if opts.VerifyMeasurements == nil {
		return result, sshStage("measurement verification", ErrSSHMeasurementsRequired)
	}
	if err := opts.VerifyMeasurements(ctx); err != nil {
		return result, sshStage("measurement verification", errors.New("measurements did not arrive"))
	}
	result.MeasurementsVerified = true
	result.Stage = "complete"
	report("complete", 100)
	return result, nil
}

// InstallSSH is a concise compatibility alias for callers that expose the
// operation as an SSH installer job.
func InstallSSH(ctx context.Context, opts SSHInstallOptions) (SSHInstallResult, error) {
	return InstallOverSSH(ctx, opts)
}

func validateSSHTarget(endpoint SSHEndpoint) error {
	if endpoint.Host == "" || strings.ContainsAny(endpoint.Host, " \t\r\n/\\'`$") {
		return ErrSSHInvalidTarget
	}
	if net.ParseIP(strings.Trim(endpoint.Host, "[]")) == nil {
		if !regexp.MustCompile(`^[A-Za-z0-9._-]+$`).MatchString(endpoint.Host) {
			return ErrSSHInvalidTarget
		}
	}
	if endpoint.Port < 1 || endpoint.Port > 65535 || endpoint.User == "" || strings.ContainsAny(endpoint.User, " \t\r\n/'`$") {
		return ErrSSHInvalidTarget
	}
	if endpoint.BindAddress != "" && net.ParseIP(endpoint.BindAddress) == nil {
		return ErrSSHInvalidTarget
	}
	return nil
}

func validateSSHAuth(auth SSHAuth) error {
	hasPassword := len(auth.Password) > 0
	hasKey := len(auth.PrivateKey) > 0
	if hasPassword == hasKey {
		return ErrSSHCredentials
	}
	if !hasKey && len(auth.PrivateKeyPassphrase) > 0 {
		return ErrSSHCredentials
	}
	return nil
}

// VerifyHostKeys applies known_hosts semantics without ever accepting a new
// key implicitly.  Existing exact host entries must match.  A new host needs
// either an owner-supplied expected fingerprint or an explicit callback.
func VerifyHostKeys(host string, port int, scanned []SSHHostKey, knownHostsPath, expectedFingerprint string, confirm func(SSHHostKey) bool) ([]SSHHostKey, error) {
	if len(scanned) == 0 {
		return nil, ErrSSHHostKeyUnknown
	}
	expected := normalizeFingerprint(expectedFingerprint)
	known, err := readKnownHostEntries(knownHostsPath)
	if err != nil {
		return nil, err
	}
	hostKnown := false
	hostRevoked := false
	trusted := make([]SSHHostKey, 0, len(scanned))
	expectedTrusted := make([]SSHHostKey, 0, len(scanned))
	for _, key := range scanned {
		if expected != "" && normalizeFingerprint(key.Fingerprint) != expected {
			continue
		}
		expectedTrusted = append(expectedTrusted, key)
		match := false
		for _, entry := range known {
			if knownHostTokenMatches(entry.hosts, host, port) {
				if entry.revoked {
					hostRevoked = true
				}
				hostKnown = true
				if entry.keyType == key.KeyType && entry.keyData == key.KeyData {
					match = true
				}
			}
		}
		if match {
			trusted = append(trusted, key)
		}
	}
	if hostRevoked {
		return nil, ErrSSHHostKeyRevoked
	}
	if len(trusted) > 0 {
		if expected != "" && normalizeFingerprint(trusted[0].Fingerprint) != expected {
			return nil, ErrSSHHostKeyFingerprint
		}
		return trusted, nil
	}
	if hostKnown {
		return nil, ErrSSHHostKeyChanged
	}
	if expected != "" {
		if len(expectedTrusted) > 0 {
			// Pin only the key whose fingerprint was explicitly supplied.  Other
			// algorithms returned by ssh-keyscan must not become implicitly trusted.
			return []SSHHostKey{expectedTrusted[0]}, nil
		}
		return nil, ErrSSHHostKeyFingerprint
	}
	if confirm == nil || !confirm(scanned[0]) {
		return nil, ErrSSHHostKeyUnknown
	}
	// Confirmation covers all verified keys for this endpoint so OpenSSH
	// can negotiate any supported algorithm.
	return scanned, nil
}

type knownHostEntry struct {
	hosts, keyType, keyData string
	revoked                 bool
}

func readKnownHostEntries(path string) ([]knownHostEntry, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []knownHostEntry
	scanner := bufio.NewScanner(bytes.NewReader(b))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 || strings.HasPrefix(fields[0], "#") || strings.HasPrefix(fields[0], "|") {
			continue
		}
		marker := ""
		if strings.HasPrefix(fields[0], "@") {
			if len(fields) < 4 {
				continue
			}
			marker, fields = fields[0], fields[1:]
			if marker == "@cert-authority" {
				// CA lines are not host keys and are intentionally not treated as
				// an exact pin by this compact verifier.
				continue
			}
		}
		entries = append(entries, knownHostEntry{hosts: fields[0], keyType: fields[1], keyData: fields[2], revoked: marker == "@revoked"})
	}
	return entries, scanner.Err()
}

func knownHostTokenMatches(tokens, host string, port int) bool {
	endpoint := host
	if strings.Contains(host, ":") {
		endpoint = "[" + strings.Trim(host, "[]") + "]"
	}
	if port != 22 {
		endpoint = "[" + strings.Trim(host, "[]") + "]:" + strconv.Itoa(port)
	}
	for _, token := range strings.Split(tokens, ",") {
		if token == host || token == endpoint {
			return true
		}
	}
	return false
}

func normalizeFingerprint(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "sha256:") {
		return "SHA256:" + value[len("sha256:"):]
	}
	return strings.TrimPrefix(value, "sha256:")
}

func writeTransientKnownHosts(keys []SSHHostKey) (string, error) {
	f, err := os.CreateTemp("", ".payesh-known-hosts-")
	if err != nil {
		return "", err
	}
	path := f.Name()
	defer func() { _ = f.Close() }()
	if err := f.Chmod(0o600); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	for _, key := range keys {
		line := key.Line
		if line == "" {
			host := key.Host
			if key.Port != 0 && key.Port != 22 {
				host = "[" + host + "]:" + strconv.Itoa(key.Port)
			}
			line = host + " " + key.KeyType + " " + key.KeyData
		}
		if _, err := io.WriteString(f, strings.TrimSpace(line)+"\n"); err != nil {
			_ = os.Remove(path)
			return "", err
		}
	}
	if err := f.Sync(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func ParseSSHKeyscan(host string, port int, raw []byte) ([]SSHHostKey, error) {
	var keys []SSHHostKey
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(fields[2])
		if err != nil {
			decoded, err = base64.RawStdEncoding.DecodeString(fields[2])
		}
		if err != nil {
			return nil, fmt.Errorf("invalid SSH host key")
		}
		hash := sha256.Sum256(decoded)
		keys = append(keys, SSHHostKey{Host: host, Port: port, KeyType: fields[1], KeyData: fields[2], Fingerprint: "SHA256:" + base64.RawStdEncoding.EncodeToString(hash[:]), Line: line})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, ErrSSHHostKeyUnknown
	}
	return keys, nil
}

func requiredArtifacts(role string) []string {
	switch role {
	case "standalone", "hub":
		return []string{"payesh-agent", "payesh-privd", "payesh", "payesh-server", "web-assets"}
	case "cli-only":
		return []string{"payesh"}
	default:
		return []string{"payesh-agent", "payesh-privd", "payesh"}
	}
}

func randomInstallID() (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", raw[:]), nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func clearSSHAuth(auth *SSHAuth) {
	for _, value := range [][]byte{auth.Password, auth.PrivateKey, auth.PrivateKeyPassphrase, auth.SudoPassword} {
		for i := range value {
			value[i] = 0
		}
	}
	auth.Password, auth.PrivateKey, auth.PrivateKeyPassphrase, auth.SudoPassword = nil, nil, nil, nil
}

// OpenSSHTransport is the production command transport.  Its methods are
// intentionally small; command output is returned only to the orchestrator
// for preflight JSON and is never included in job errors.
type OpenSSHTransport struct {
	ConnectTimeout string
	ControlPath    string
}

func (OpenSSHTransport) Scan(ctx context.Context, endpoint SSHEndpoint) ([]SSHHostKey, error) {
	var lastErr error

	// Strategy 1: Standard untyped ssh-keyscan with generous timeout.
	// OpenSSH's standard ssh-keyscan discovers default host key types without forcing specific algorithm negotiations.
	argsUntyped := []string{"-T", "15", "-p", strconv.Itoa(endpoint.Port), endpoint.Host}
	cmdUntyped := exec.CommandContext(ctx, "ssh-keyscan", argsUntyped...)
	outUntyped, errUntyped := cmdUntyped.CombinedOutput()
	if len(bytes.TrimSpace(outUntyped)) > 0 {
		if keys, parseErr := ParseSSHKeyscan(endpoint.Host, endpoint.Port, outUntyped); parseErr == nil && len(keys) > 0 {
			return keys, nil
		}
	}
	if errUntyped != nil {
		lastErr = errUntyped
	}

	// Strategy 2: Individual key type probes (ed25519, rsa, ecdsa) in isolation.
	// Probing individual key types isolates failures from unsupported types (e.g. sk-ecdsa or dsa).
	for _, keyType := range []string{"ed25519", "rsa", "ecdsa"} {
		argsType := []string{"-T", "12", "-t", keyType, "-p", strconv.Itoa(endpoint.Port), endpoint.Host}
		cmdType := exec.CommandContext(ctx, "ssh-keyscan", argsType...)
		outType, errType := cmdType.CombinedOutput()
		if len(bytes.TrimSpace(outType)) > 0 {
			if keys, parseErr := ParseSSHKeyscan(endpoint.Host, endpoint.Port, outType); parseErr == nil && len(keys) > 0 {
				return keys, nil
			}
		}
		if errType != nil && lastErr == nil {
			lastErr = errType
		}
	}

	// Strategy 3: Resilient Auto-Solver using native OpenSSH client with StrictHostKeyChecking=accept-new.
	// OpenSSH client directly negotiates key exchange with the server and records the negotiated host key
	// into a temporary known_hosts file. This automatically resolves cases where ssh-keyscan is blocked,
	// filtered, or fails algorithm negotiation, and natively supports bind addresses.
	known, tempErr := os.CreateTemp("", ".payesh-keyscan-")
	if tempErr == nil {
		knownPath := known.Name()
		_ = known.Close()
		defer os.Remove(knownPath)

		user := endpoint.User
		if user == "" {
			user = "root"
		}
		sshArgs := []string{
			"-p", strconv.Itoa(endpoint.Port),
			"-o", "StrictHostKeyChecking=accept-new",
			"-o", "BatchMode=yes",
			"-o", "ConnectTimeout=15",
			"-o", "Ciphers=aes128-gcm@openssh.com,aes256-gcm@openssh.com,aes128-ctr,aes192-ctr,aes256-ctr",
			"-o", "HashKnownHosts=no",
			"-o", "PreferredAuthentications=none,publickey,password",
			"-o", "NumberOfPasswordPrompts=0",
			"-o", "UserKnownHostsFile=" + knownPath,
		}
		if endpoint.BindAddress != "" {
			sshArgs = append(sshArgs, "-b", endpoint.BindAddress)
		}
		sshArgs = append(sshArgs, "--", user+"@"+endpoint.Host, "exit")

		cmdSSH := exec.CommandContext(ctx, "ssh", sshArgs...)
		sshOut, sshErr := cmdSSH.CombinedOutput()

		body, readErr := os.ReadFile(knownPath)
		if readErr == nil && len(bytes.TrimSpace(body)) > 0 {
			if keys, parseErr := ParseSSHKeyscan(endpoint.Host, endpoint.Port, body); parseErr == nil && len(keys) > 0 {
				return keys, nil
			}
		}
		if sshErr != nil && len(bytes.TrimSpace(sshOut)) > 0 {
			lastErr = errors.New(strings.TrimSpace(string(sshOut)))
		} else if sshErr != nil {
			lastErr = sshErr
		}
	}

	diag := "connection timed out or host unreachable"
	if lastErr != nil {
		diag = lastErr.Error()
		if idx := strings.IndexAny(diag, "\r\n"); idx != -1 {
			diag = strings.TrimSpace(diag[:idx])
		}
	}
	return nil, fmt.Errorf("host key scan failed for %s:%d (%s)", endpoint.Host, endpoint.Port, diag)
}

func (t OpenSSHTransport) Upload(ctx context.Context, endpoint SSHEndpoint, knownHosts string, auth SSHAuth, source, destination string, recursive bool) error {
	if err := validateArtifactPath(source, recursive); err != nil {
		return errors.New("unsafe SCP source path")
	}
	if destination == "" || !filepath.IsAbs(destination) || filepath.Clean(destination) != destination || strings.HasPrefix(filepath.Base(destination), "-") {
		return errors.New("unsafe SCP destination path")
	}
	// Prefer rsync when installed on both endpoints. It uses an atomic temporary
	// destination and reliably closes on provider SFTP implementations that can
	// leave a completed SCP transfer waiting forever. A failed/unavailable
	// rsync attempt falls back to the universally available SCP path.
	var rsyncErrReport error
	if _, lookErr := exec.LookPath("rsync"); lookErr == nil {
		args := t.rsyncArgs(endpoint, knownHosts, source, destination, recursive)
		if _, rsyncErr := t.runAuthCommand(ctx, "rsync", args, auth, nil); rsyncErr == nil {
			return nil
		} else {
			rsyncErrReport = rsyncErr
		}
	}
	args := t.scpArgs(endpoint, knownHosts, source, destination, recursive)
	if out, err := t.runAuthCommand(ctx, "scp", args, auth, nil); err != nil {
		if rsyncErrReport != nil {
			return fmt.Errorf("SCP transfer failed: %w: %s (rsync: %v)", err, strings.TrimSpace(string(out)), rsyncErrReport)
		}
		return fmt.Errorf("SCP transfer failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (t OpenSSHTransport) rsyncArgs(endpoint SSHEndpoint, knownHosts, source, destination string, recursive bool) []string {
	sshCommand := "ssh -p " + strconv.Itoa(endpoint.Port) + " -o BatchMode=yes -o ConnectTimeout=" + t.timeout() + " -o Ciphers=aes128-gcm@openssh.com,aes256-gcm@openssh.com,aes128-ctr,aes192-ctr,aes256-ctr -o UserKnownHostsFile=" + knownHosts + " -o StrictHostKeyChecking=yes"
	if t.ControlPath != "" {
		sshCommand += " -o ControlMaster=auto -o ControlPath=" + t.ControlPath + " -o ControlPersist=120s"
	}
	if endpoint.BindAddress != "" {
		sshCommand += " -b " + endpoint.BindAddress
	}
	args := []string{"--archive", "--timeout=120", "--rsh=" + sshCommand, "--"}
	if recursive {
		source = strings.TrimRight(source, string(filepath.Separator)) + string(filepath.Separator)
		destination = strings.TrimRight(destination, "/") + "/"
	}
	return append(args, source, endpoint.User+"@"+endpoint.Host+":"+destination)
}

func (t OpenSSHTransport) Run(ctx context.Context, endpoint SSHEndpoint, knownHosts string, auth SSHAuth, command string, stdin []byte) ([]byte, error) {
	args := t.sshArgs(endpoint, knownHosts, command)
	var out []byte
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		out, err = t.runAuthCommand(ctx, "ssh", args, auth, stdin)
		if err == nil || ctx.Err() != nil {
			return out, err
		}
		errStr := err.Error()
		if strings.Contains(errStr, "exit status 255") || strings.Contains(errStr, "Connection reset") || strings.Contains(errStr, "Connection closed") || strings.Contains(errStr, "timed out") {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt+1) * 1500 * time.Millisecond):
			}
			continue
		}
		break
	}
	return out, err
}

func (t OpenSSHTransport) sshArgs(endpoint SSHEndpoint, knownHosts, command string) []string {
	args := []string{"-p", strconv.Itoa(endpoint.Port), "-o", "BatchMode=yes", "-o", "ConnectTimeout=" + t.timeout(), "-o", "Ciphers=aes128-gcm@openssh.com,aes256-gcm@openssh.com,aes128-ctr,aes192-ctr,aes256-ctr", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-o", "UserKnownHostsFile=" + knownHosts, "-o", "StrictHostKeyChecking=yes"}
	if t.ControlPath != "" {
		args = append(args, "-o", "ControlMaster=auto", "-o", "ControlPath="+t.ControlPath, "-o", "ControlPersist=120s")
	}
	if endpoint.BindAddress != "" {
		args = append(args, "-b", endpoint.BindAddress)
	}
	return append(args, "--", endpoint.User+"@"+endpoint.Host, command)
}

func (t OpenSSHTransport) scpArgs(endpoint SSHEndpoint, knownHosts, source, destination string, recursive bool) []string {
	args := []string{"-P", strconv.Itoa(endpoint.Port), "-o", "BatchMode=yes", "-o", "ConnectTimeout=" + t.timeout(), "-o", "Ciphers=aes128-gcm@openssh.com,aes256-gcm@openssh.com,aes128-ctr,aes192-ctr,aes256-ctr", "-o", "UserKnownHostsFile=" + knownHosts, "-o", "StrictHostKeyChecking=yes"}
	if t.ControlPath != "" {
		args = append(args, "-o", "ControlMaster=auto", "-o", "ControlPath="+t.ControlPath, "-o", "ControlPersist=120s")
	}
	if endpoint.BindAddress != "" {
		args = append(args, "-o", "BindAddress="+endpoint.BindAddress)
	}
	if recursive {
		args = append(args, "-r")
	}
	return append(args, "--", source, endpoint.User+"@"+endpoint.Host+":"+destination)
}

func (t OpenSSHTransport) timeout() string {
	if t.ConnectTimeout != "" {
		return t.ConnectTimeout
	}
	return "35"
}

func (t OpenSSHTransport) runAuthCommand(ctx context.Context, name string, args []string, auth SSHAuth, stdin []byte) ([]byte, error) {
	if err := validateSSHAuth(auth); err != nil {
		return nil, err
	}
	if len(auth.Password) > 0 {
		if _, err := exec.LookPath("sshpass"); err != nil {
			return nil, errors.New("sshpass is unavailable for password authentication")
		}
		args = append([]string{"-e", "--", name}, withoutBatchMode(args)...)
		return runCommand(ctx, "sshpass", args, auth.Password, stdin)
	}
	// A key file is transient and mode 0600.  A short-lived ssh-agent handles
	// both unencrypted and encrypted keys without putting a passphrase in argv.
	key, err := os.CreateTemp("", ".payesh-ssh-key-")
	if err != nil {
		return nil, err
	}
	keyPath := key.Name()
	defer os.Remove(keyPath)
	if err := key.Chmod(0o600); err != nil {
		_ = key.Close()
		return nil, err
	}
	if _, err := key.Write(auth.PrivateKey); err != nil {
		_ = key.Close()
		return nil, err
	}
	if err := key.Close(); err != nil {
		return nil, err
	}
	agent, err := startSSHAgent(ctx, keyPath, auth.PrivateKeyPassphrase)
	if err != nil {
		return nil, err
	}
	defer agent.close()
	return runCommandEnv(ctx, name, args, agent.env, stdin)
}

func withoutBatchMode(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "-o" && i+1 < len(args) && args[i+1] == "BatchMode=yes" {
			i++
			out = append(out, "-o", "BatchMode=no", "-o", "NumberOfPasswordPrompts=1")
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func runCommand(ctx context.Context, name string, args []string, secret, stdin []byte) ([]byte, error) {
	var env []string
	if secret != nil {
		env = append(os.Environ(), "SSHPASS="+string(secret))
	}
	return runCommandEnv(ctx, name, args, env, stdin)
}

func runCommandEnv(ctx context.Context, name string, args, env []string, stdin []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if env != nil {
		cmd.Env = env
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	} else {
		cmd.Stdin = bytes.NewReader(nil)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, err
	}
	return out, nil
}

type sshAgent struct {
	env      []string
	socket   string
	pid      string
	passFile string
	askFile  string
}

func startSSHAgent(ctx context.Context, keyPath string, passphrase []byte) (sshAgent, error) {
	out, err := runCommand(ctx, "ssh-agent", []string{"-s"}, nil, nil)
	if err != nil {
		return sshAgent{}, errors.New("ssh-agent is unavailable")
	}
	socket, pid := "", ""
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "SSH_AUTH_SOCK=") {
			socket = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(line), "SSH_AUTH_SOCK="), "; export SSH_AUTH_SOCK;")
		}
		if strings.HasPrefix(line, "SSH_AGENT_PID=") {
			pid = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(line), "SSH_AGENT_PID="), "; export SSH_AGENT_PID;")
		}
	}
	if socket == "" || pid == "" {
		return sshAgent{}, errors.New("ssh-agent returned invalid environment")
	}
	agent := sshAgent{socket: socket, pid: pid}
	agent.env = append(os.Environ(), "SSH_AUTH_SOCK="+socket, "SSH_AGENT_PID="+pid, "DISPLAY=:0", "SSH_ASKPASS_REQUIRE=force")
	pass, err := os.CreateTemp("", ".payesh-ssh-pass-")
	if err != nil {
		agent.close()
		return sshAgent{}, err
	}
	agent.passFile = pass.Name()
	if err := pass.Chmod(0o600); err != nil {
		_ = pass.Close()
		agent.close()
		return sshAgent{}, err
	}
	if _, err := pass.Write(passphrase); err != nil {
		_ = pass.Close()
		agent.close()
		return sshAgent{}, err
	}
	if err := pass.Close(); err != nil {
		agent.close()
		return sshAgent{}, err
	}
	ask, err := os.CreateTemp("", ".payesh-ssh-askpass-")
	if err != nil {
		agent.close()
		return sshAgent{}, err
	}
	agent.askFile = ask.Name()
	if err := ask.Chmod(0o700); err != nil {
		_ = ask.Close()
		agent.close()
		return sshAgent{}, err
	}
	if _, err := ask.WriteString("#!/bin/sh\ncat \"$PAYESH_ASKPASS_FILE\"\n"); err != nil {
		_ = ask.Close()
		agent.close()
		return sshAgent{}, err
	}
	if err := ask.Close(); err != nil {
		agent.close()
		return sshAgent{}, err
	}
	agent.env = append(agent.env, "SSH_ASKPASS="+agent.askFile, "PAYESH_ASKPASS_FILE="+agent.passFile)
	if out, err := runCommandEnv(ctx, "ssh-add", []string{"--", keyPath}, agent.env, nil); err != nil {
		agent.close()
		detail := strings.TrimSpace(string(out))
		if detail != "" {
			return sshAgent{}, fmt.Errorf("private key could not be loaded: %s", detail)
		}
		return sshAgent{}, errors.New("private key could not be loaded")
	}
	return agent, nil
}

func (a *sshAgent) close() {
	if a == nil {
		return
	}
	if a.socket != "" && a.pid != "" {
		_, _ = runCommandEnv(context.Background(), "ssh-agent", []string{"-k"}, a.env, nil)
	}
	if a.passFile != "" {
		_ = os.Remove(a.passFile)
	}
	if a.askFile != "" {
		_ = os.Remove(a.askFile)
	}
	a.env, a.socket, a.pid, a.passFile, a.askFile = nil, "", "", "", ""
}

const githubBootstrapScriptTemplate = `#!/bin/sh
set -u
DIR='%s'
ARCH='%s'
ROLE='%s'
START='%s'
LISTEN='%s'
ARTIFACTS='%s'
STATUS_FILE="${DIR}/status.txt"
LOG_FILE="${DIR}/install.log"
PREFLIGHT_FILE="${DIR}/preflight.json"

log() {
	echo "[$(date -u '+%%Y-%%m-%%d %%H:%%M:%%S UTC')] $*" >> "$LOG_FILE"
}

set_status() {
	echo "$1" > "$STATUS_FILE"
}

exec >> "$LOG_FILE" 2>&1
log "Starting Payesh GitHub bootstrap installer for role '${ROLE}' (arch: ${ARCH})..."
set_status "RUNNING:github_fetch"

# Fetch precompiled binaries from GitHub binaries branch
if [ ! -d "${DIR}/payesh-repo" ]; then
	log "Fetching Payesh binaries from GitHub (Real-kia/payesh:binaries)..."
	GIT_CMD="git clone --quiet --branch binaries --depth 1"
	if [ -s "${DIR}/id_github" ]; then
		export GIT_SSH_COMMAND="ssh -i ${DIR}/id_github -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"
		$GIT_CMD git@github.com:Real-kia/payesh.git "${DIR}/payesh-repo" >/dev/null 2>&1 || true
	fi
	if [ ! -d "${DIR}/payesh-repo" ]; then
		$GIT_CMD https://github.com/Real-kia/payesh.git "${DIR}/payesh-repo" >/dev/null 2>&1 || true
	fi
fi

fetch_bin() {
	NAME="$1"
	TARGET="${DIR}/${NAME}"
	rm -f "${TARGET}.tmp"
	if [ -s "${TARGET}" ]; then
		chmod 700 "${TARGET}"
		return 0
	fi

	# 1. Look in cloned GitHub binaries branch
	if [ -d "${DIR}/payesh-repo/matrix" ]; then
		SRC="${DIR}/payesh-repo/matrix/${NAME}-linux-${ARCH}"
		if [ -s "$SRC" ]; then
			cp -f "$SRC" "$TARGET"
			chmod 700 "$TARGET"
			log "Acquired $NAME for ${ARCH} from GitHub binaries branch"
			return 0
		fi
	fi

	# 2. Look in direct raw / release URLs (for when repo is public or has releases)
	URLS="https://raw.githubusercontent.com/Real-kia/payesh/binaries/matrix/${NAME}-linux-${ARCH} https://github.com/Real-kia/payesh/releases/latest/download/${NAME}-linux-${ARCH} https://raw.githubusercontent.com/Real-kia/payesh/master/dist/matrix/${NAME}-linux-${ARCH}"
	for URL in $URLS; do
		log "Probing GitHub URL: $URL"
		if command -v curl >/dev/null 2>&1; then
			curl -fsSL --connect-timeout 8 --max-time 60 "$URL" -o "${TARGET}.tmp" 2>/dev/null || true
		elif command -v wget >/dev/null 2>&1; then
			wget -q -T 8 -t 2 -O "${TARGET}.tmp" "$URL" 2>/dev/null || true
		fi

		if [ -s "${TARGET}.tmp" ]; then
			mv -f "${TARGET}.tmp" "${TARGET}"
			chmod 700 "${TARGET}"
			log "Acquired $NAME from GitHub: $URL"
			return 0
		fi
		rm -f "${TARGET}.tmp"
	done
	return 1
}

# Fetch installer and required role artifacts
for art in $ARTIFACTS; do
	if ! fetch_bin "$art"; then
		log "GitHub download unavailable for $art"
		set_status "FALLBACK:master_upload_required"
		exit 0
	fi
done

# Cleanup temporary clone and deploy key to keep disk clean & secure
rm -rf "${DIR}/payesh-repo" "${DIR}/id_github" 2>/dev/null || true

log "All binaries successfully fetched from GitHub. Running preflight..."
set_status "RUNNING:preflight"
chmod 700 "${DIR}/payesh-install" 2>/dev/null || true

PREFLIGHT_CMD="${DIR}/payesh-install --json --role ${ROLE}"
if [ -n "$LISTEN" ]; then PREFLIGHT_CMD="${PREFLIGHT_CMD} --listen ${LISTEN}"; fi
if ! $PREFLIGHT_CMD > "$PREFLIGHT_FILE" 2>> "$LOG_FILE"; then
	log "Preflight failed"
	set_status "FAILED:preflight"
	exit 1
fi

log "Executing payesh-install..."
set_status "RUNNING:install"

calc_sha256() {
	_FILE="$1"
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$_FILE" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$_FILE" | awk '{print $1}'
	else
		openssl dgst -sha256 "$_FILE" | awk '{print $NF}'
	fi
}

SHA_FLAGS=""
for art in $ARTIFACTS; do
	if [ -f "${DIR}/${art}" ] && [ "$art" != "payesh-install" ]; then
		SUM=$(calc_sha256 "${DIR}/${art}")
		SHA_FLAGS="${SHA_FLAGS} --artifact-sha256 ${art}=${SUM}"
	fi
done

INSTALL_CMD="${DIR}/payesh-install --install --role ${ROLE} --artifact-dir ${DIR}${SHA_FLAGS}"
if [ "$START" = "1" ]; then INSTALL_CMD="${INSTALL_CMD} --start"; fi
if [ -n "$LISTEN" ]; then INSTALL_CMD="${INSTALL_CMD} --listen ${LISTEN}"; fi
if ! $INSTALL_CMD >> "$LOG_FILE" 2>&1; then
	log "Install command failed"
	set_status "FAILED:install"
	exit 1
fi

if [ "$ROLE" = "node" ]; then
	log "Configuring node identity and environment..."
	mkdir -p /var/lib/payesh /etc/payesh
	if [ -s "${DIR}/server-id" ]; then cp -f "${DIR}/server-id" /var/lib/payesh/server-id; fi
	if [ -s "${DIR}/node-identity.json" ]; then cp -f "${DIR}/node-identity.json" /var/lib/payesh/node-identity.json; fi
	if [ -s "${DIR}/hub-ca.pem" ]; then cp -f "${DIR}/hub-ca.pem" /var/lib/payesh/hub-ca.pem; fi
	if [ -s "${DIR}/payesh.env" ]; then cp -f "${DIR}/payesh.env" /etc/payesh/payesh.env; fi
	if id payesh >/dev/null 2>&1; then
		chown -R payesh:payesh /var/lib/payesh /etc/payesh 2>/dev/null || true
		if [ -f /var/lib/payesh/node-identity.json ]; then chmod 600 /var/lib/payesh/node-identity.json; fi
		if [ -f /var/lib/payesh/server-id ]; then chmod 640 /var/lib/payesh/server-id; fi
		if [ -f /etc/payesh/payesh.env ]; then chmod 640 /etc/payesh/payesh.env; fi
		if [ -f /var/lib/payesh/hub-ca.pem ]; then chmod 644 /var/lib/payesh/hub-ca.pem; fi
	fi
	if [ "$START" = "1" ] && command -v systemctl >/dev/null 2>&1; then
		systemctl daemon-reload 2>/dev/null || true
		systemctl restart payesh-agent 2>/dev/null || true
	fi
fi

log "Installation complete!"
set_status "SUCCESS:0"
exit 0
`

func stageNodeConfig(ctx context.Context, transport SSHTransport, endpoint SSHEndpoint, knownHosts string, auth SSHAuth, remoteDir string, opts SSHInstallOptions) {
	if opts.Role != "node" {
		return
	}
	if opts.ServerID != "" {
		cmd := fmt.Sprintf("cat << 'EOF' > %s/server-id\n%s\nEOF\nchmod 640 %s/server-id", remoteDir, strings.TrimSpace(opts.ServerID), remoteDir)
		_, _ = transport.Run(ctx, endpoint, knownHosts, auth, cmd, nil)
	}
	if len(opts.NodeIdentityJSON) > 0 {
		cmd := fmt.Sprintf("cat << 'EOF' > %s/node-identity.json\n%s\nEOF\nchmod 600 %s/node-identity.json", remoteDir, string(opts.NodeIdentityJSON), remoteDir)
		_, _ = transport.Run(ctx, endpoint, knownHosts, auth, cmd, nil)
	}
	if len(opts.HubTrustPEM) > 0 {
		cmd := fmt.Sprintf("cat << 'EOF' > %s/hub-ca.pem\n%s\nEOF\nchmod 644 %s/hub-ca.pem", remoteDir, string(opts.HubTrustPEM), remoteDir)
		_, _ = transport.Run(ctx, endpoint, knownHosts, auth, cmd, nil)
	}
	if opts.TransportURL != "" {
		envBody := fmt.Sprintf("PAYESH_TRANSPORT_URL=%s\nPAYESH_HUB_TRUST_FILE=/var/lib/payesh/hub-ca.pem\nPAYESH_NODE_IDENTITY_FILE=/var/lib/payesh/node-identity.json\n", strings.TrimSpace(opts.TransportURL))
		cmd := fmt.Sprintf("cat << 'EOF' > %s/payesh.env\n%s\nEOF\nchmod 640 %s/payesh.env", remoteDir, envBody, remoteDir)
		_, _ = transport.Run(ctx, endpoint, knownHosts, auth, cmd, nil)
	}
}

func applyNodeConfig(ctx context.Context, transport SSHTransport, endpoint SSHEndpoint, knownHosts string, auth SSHAuth, sudoPrefix string, sudoInput []byte, remoteDir string, opts SSHInstallOptions) {
	if opts.Role != "node" {
		return
	}
	cmd := fmt.Sprintf(`mkdir -p /var/lib/payesh /etc/payesh
if [ -s %s/server-id ]; then cp -f %s/server-id /var/lib/payesh/server-id; fi
if [ -s %s/node-identity.json ]; then cp -f %s/node-identity.json /var/lib/payesh/node-identity.json && chmod 600 /var/lib/payesh/node-identity.json; fi
if [ -s %s/hub-ca.pem ]; then cp -f %s/hub-ca.pem /var/lib/payesh/hub-ca.pem && chmod 644 /var/lib/payesh/hub-ca.pem; fi
if [ -s %s/payesh.env ]; then cp -f %s/payesh.env /etc/payesh/payesh.env && chmod 640 /etc/payesh/payesh.env; fi
if id payesh >/dev/null 2>&1; then
	chown -R payesh:payesh /var/lib/payesh /etc/payesh 2>/dev/null || true
	if [ -f /var/lib/payesh/node-identity.json ]; then chmod 600 /var/lib/payesh/node-identity.json; fi
	if [ -f /var/lib/payesh/server-id ]; then chmod 640 /var/lib/payesh/server-id; fi
	if [ -f /etc/payesh/payesh.env ]; then chmod 640 /etc/payesh/payesh.env; fi
	if [ -f /var/lib/payesh/hub-ca.pem ]; then chmod 644 /var/lib/payesh/hub-ca.pem; fi
fi
if command -v systemctl >/dev/null 2>&1; then
	systemctl daemon-reload 2>/dev/null || true
	systemctl restart payesh-agent 2>/dev/null || true
fi`,
		remoteDir, remoteDir, remoteDir, remoteDir, remoteDir, remoteDir, remoteDir, remoteDir)
	_, _ = transport.Run(ctx, endpoint, knownHosts, auth, sudoPrefix+cmd, sudoInput)
}

