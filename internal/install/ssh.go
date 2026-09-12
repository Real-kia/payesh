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
	Host string
	Port int
	User string
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

	Transport SSHTransport
	// These callbacks represent the authenticated transport/enrollment
	// boundaries.  Success is not reported until both are present and succeed.
	Enroll             func(context.Context) error
	VerifyMeasurements func(context.Context) error
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
		artifactPaths[name] = path
	}
	transport := opts.Transport
	if transport == nil {
		transport = OpenSSHTransport{}
	}

	result.Stage = "host-key"
	keys, scanErr := transport.Scan(ctx, opts.Endpoint)
	if scanErr != nil {
		return result, sshStage("scan host key", scanErr)
	}
	trusted, err := VerifyHostKeys(opts.Endpoint.Host, opts.Endpoint.Port, keys, opts.KnownHostsPath, opts.ExpectedHostKeyFingerprint, opts.ConfirmHostKey)
	if err != nil {
		return result, sshStage("verify host key", err)
	}
	result.HostKeys = append([]SSHHostKey(nil), trusted...)
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
	if err = transport.Upload(ctx, opts.Endpoint, knownHosts, opts.Auth, opts.InstallerPath, remoteDir+"/payesh-install", false); err != nil {
		return result, sshStage("upload installer", err)
	}
	for _, name := range requiredArtifacts(opts.Role) {
		path := artifactPaths[name]
		recursive := name == "web-assets"
		if err = transport.Upload(ctx, opts.Endpoint, knownHosts, opts.Auth, path, remoteDir+"/"+name, recursive); err != nil {
			return result, sshStage("upload artifact", err)
		}
	}
	if _, err = transport.Run(ctx, opts.Endpoint, knownHosts, opts.Auth, "chmod 700 -- "+shellQuote(remoteDir+"/payesh-install"), nil); err != nil {
		return result, sshStage("prepare installer", err)
	}

	// The remote preflight is parsed but never treated as successful install.
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

	installCommand := shellQuote(remoteDir+"/payesh-install") + " --install --role " + shellQuote(opts.Role) + " --artifact-dir " + shellQuote(remoteDir)
	if opts.Start {
		installCommand += " --start"
	}
	if opts.Listen != "" {
		installCommand += " --listen " + shellQuote(opts.Listen)
	}
	if _, err = transport.Run(ctx, opts.Endpoint, knownHosts, opts.Auth, sudoPrefix+installCommand, sudoInput); err != nil {
		return result, sshStage("remote install", err)
	}
	result.Installed = append([]string(nil), result.Preflight.Artifacts...)
	if opts.Enroll == nil {
		return result, sshStage("enrollment", ErrSSHEnrollmentRequired)
	}
	if err := opts.Enroll(ctx); err != nil {
		return result, sshStage("enrollment", errors.New("enrollment failed"))
	}
	result.Enrolled = true
	if opts.VerifyMeasurements == nil {
		return result, sshStage("measurement verification", ErrSSHMeasurementsRequired)
	}
	if err := opts.VerifyMeasurements(ctx); err != nil {
		return result, sshStage("measurement verification", errors.New("measurements did not arrive"))
	}
	result.MeasurementsVerified = true
	result.Stage = "complete"
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
	// Confirmation covers the displayed key only.  OpenSSH will fail closed if
	// the server negotiates a different unconfirmed algorithm.
	return []SSHHostKey{scanned[0]}, nil
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
}

func (OpenSSHTransport) Scan(ctx context.Context, endpoint SSHEndpoint) ([]SSHHostKey, error) {
	args := []string{"-T", "10", "-p", strconv.Itoa(endpoint.Port), endpoint.Host}
	out, err := runCommand(ctx, "ssh-keyscan", args, nil, nil)
	if err != nil {
		return nil, errors.New("ssh-keyscan failed")
	}
	return ParseSSHKeyscan(endpoint.Host, endpoint.Port, out)
}

func (t OpenSSHTransport) Upload(ctx context.Context, endpoint SSHEndpoint, knownHosts string, auth SSHAuth, source, destination string, recursive bool) error {
	if err := validateArtifactPath(source, recursive); err != nil {
		return errors.New("unsafe SCP source path")
	}
	if destination == "" || !filepath.IsAbs(destination) || filepath.Clean(destination) != destination || strings.HasPrefix(filepath.Base(destination), "-") {
		return errors.New("unsafe SCP destination path")
	}
	args := t.scpArgs(endpoint, knownHosts, source, destination, recursive)
	if _, err := t.runAuthCommand(ctx, "scp", args, auth, nil); err != nil {
		return errors.New("SCP transfer failed")
	}
	return nil
}

func (t OpenSSHTransport) Run(ctx context.Context, endpoint SSHEndpoint, knownHosts string, auth SSHAuth, command string, stdin []byte) ([]byte, error) {
	args := t.sshArgs(endpoint, knownHosts, command)
	return t.runAuthCommand(ctx, "ssh", args, auth, stdin)
}

func (t OpenSSHTransport) sshArgs(endpoint SSHEndpoint, knownHosts, command string) []string {
	return []string{"-p", strconv.Itoa(endpoint.Port), "-o", "BatchMode=yes", "-o", "ConnectTimeout=" + t.timeout(), "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-o", "UserKnownHostsFile=" + knownHosts, "-o", "StrictHostKeyChecking=yes", "--", endpoint.User + "@" + endpoint.Host, command}
}

func (t OpenSSHTransport) scpArgs(endpoint SSHEndpoint, knownHosts, source, destination string, recursive bool) []string {
	args := []string{"-P", strconv.Itoa(endpoint.Port), "-o", "BatchMode=yes", "-o", "ConnectTimeout=" + t.timeout(), "-o", "UserKnownHostsFile=" + knownHosts, "-o", "StrictHostKeyChecking=yes"}
	if recursive {
		args = append(args, "-r")
	}
	return append(args, "--", source, endpoint.User+"@"+endpoint.Host+":"+destination)
}

func (t OpenSSHTransport) timeout() string {
	if t.ConnectTimeout != "" {
		return t.ConnectTimeout
	}
	return "20"
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
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
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
	if _, err := runCommandEnv(ctx, "ssh-add", []string{"--", keyPath}, agent.env, nil); err != nil {
		agent.close()
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
