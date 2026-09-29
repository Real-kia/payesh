package install

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testHostKey(host string, port int, value byte) SSHHostKey {
	blob := []byte{0, 0, 0, 11, 's', 's', 'h', '-', 'e', 'd', '2', '5', '5', '1', value}
	data := base64.StdEncoding.EncodeToString(blob)
	sum := sha256.Sum256(blob)
	return SSHHostKey{Host: host, Port: port, KeyType: "ssh-ed25519", KeyData: data, Fingerprint: "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]), Line: host + " ssh-ed25519 " + data}
}

func TestVerifyHostKeysRequiresExplicitTrustAndRejectsChangedKey(t *testing.T) {
	key := testHostKey("node.example", 2222, 1)
	if _, err := VerifyHostKeys(key.Host, key.Port, []SSHHostKey{key}, "", "", nil); !errors.Is(err, ErrSSHHostKeyUnknown) {
		t.Fatalf("unknown host err=%v", err)
	}
	called := false
	trusted, err := VerifyHostKeys(key.Host, key.Port, []SSHHostKey{key}, "", "", func(got SSHHostKey) bool {
		called = true
		return got.Fingerprint == key.Fingerprint
	})
	if err != nil || !called || len(trusted) != 1 {
		t.Fatalf("confirmed host trusted=%v called=%v err=%v", trusted, called, err)
	}
	known := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(known, []byte("[node.example]:2222 ssh-ed25519 "+key.KeyData+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed := testHostKey("node.example", 2222, 2)
	if _, err := VerifyHostKeys(changed.Host, changed.Port, []SSHHostKey{changed}, known, "", nil); !errors.Is(err, ErrSSHHostKeyChanged) {
		t.Fatalf("changed host err=%v", err)
	}
}

func TestVerifyHostKeysHonorsRevokedEntry(t *testing.T) {
	key := testHostKey("node.example", 22, 9)
	known := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(known, []byte("@revoked node.example ssh-ed25519 "+key.KeyData+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyHostKeys(key.Host, key.Port, []SSHHostKey{key}, known, "", nil); !errors.Is(err, ErrSSHHostKeyRevoked) {
		t.Fatalf("revoked host err=%v", err)
	}
}

func TestParseSSHKeyscanFingerprintAndPort(t *testing.T) {
	key := testHostKey("[::1]:2200", 2200, 3)
	parsed, err := ParseSSHKeyscan("::1", 2200, []byte(key.Line+" comment\n"))
	if err != nil || len(parsed) != 1 {
		t.Fatalf("parsed=%v err=%v", parsed, err)
	}
	if parsed[0].Fingerprint != key.Fingerprint || parsed[0].KeyType != key.KeyType {
		t.Fatalf("parsed key=%+v want=%+v", parsed[0], key)
	}
}

func TestOpenSSHArgumentsIncludeValidatedBindAddress(t *testing.T) {
	endpoint := SSHEndpoint{Host: "node.example", Port: 2222, User: "root", BindAddress: "192.0.2.10"}
	if err := validateSSHTarget(endpoint); err != nil {
		t.Fatal(err)
	}
	transport := OpenSSHTransport{}
	if got := strings.Join(transport.sshArgs(endpoint, "/tmp/known", "true"), " "); !strings.Contains(got, "-b 192.0.2.10") {
		t.Fatalf("SSH bind address missing: %s", got)
	}
	if got := strings.Join(transport.scpArgs(endpoint, "/tmp/known", "/tmp/source", "/tmp/destination", false), " "); !strings.Contains(got, "BindAddress=192.0.2.10") {
		t.Fatalf("SCP bind address missing: %s", got)
	}
	if got := strings.Join(transport.rsyncArgs(endpoint, "/tmp/known", "/tmp/source", "/tmp/destination", false), " "); !strings.Contains(got, "-b 192.0.2.10") || !strings.Contains(got, "--timeout=120") {
		t.Fatalf("rsync safety/bind arguments missing: %s", got)
	}
	endpoint.BindAddress = "not-an-ip"
	if err := validateSSHTarget(endpoint); !errors.Is(err, ErrSSHInvalidTarget) {
		t.Fatalf("invalid bind address err=%v", err)
	}
}

type fakeSSHTransport struct {
	keys      []SSHHostKey
	preflight Preflight
	commands  []string
	uploads   []string
	authSeen  []byte
}

func (f *fakeSSHTransport) Scan(context.Context, SSHEndpoint) ([]SSHHostKey, error) {
	return f.keys, nil
}

func (f *fakeSSHTransport) Upload(_ context.Context, _ SSHEndpoint, _ string, auth SSHAuth, source, destination string, recursive bool) error {
	f.uploads = append(f.uploads, source+"=>"+destination+":"+strconvBool(recursive))
	if len(auth.Password) > 0 {
		f.authSeen = append([]byte(nil), auth.Password...)
	}
	return nil
}

func (f *fakeSSHTransport) Run(_ context.Context, _ SSHEndpoint, _ string, _ SSHAuth, command string, _ []byte) ([]byte, error) {
	f.commands = append(f.commands, command)
	if strings.Contains(command, " --json ") {
		return json.Marshal(f.preflight)
	}
	return nil, nil
}

func strconvBool(value bool) string {
	if value {
		return "recursive"
	}
	return "file"
}

func TestInstallOverSSHUsesTransientCredentialsAndRequiresMeasurement(t *testing.T) {
	root := t.TempDir()
	installer := filepath.Join(root, "payesh-install")
	if err := os.WriteFile(installer, []byte("installer"), 0o700); err != nil {
		t.Fatal(err)
	}
	artifacts := map[string]string{}
	for _, name := range requiredArtifacts("node") {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(name), 0o700); err != nil {
			t.Fatal(err)
		}
		artifacts[name] = path
	}
	key := testHostKey("node.example", 2222, 4)
	transport := &fakeSSHTransport{keys: []SSHHostKey{key}, preflight: Preflight{Role: "node", Supported: true, Artifacts: requiredArtifacts("node")}}
	password := []byte("temporary-password")
	result, err := InstallOverSSH(context.Background(), SSHInstallOptions{
		Endpoint:                   SSHEndpoint{Host: "node.example", Port: 2222, User: "root"},
		ExpectedHostKeyFingerprint: key.Fingerprint,
		Auth:                       SSHAuth{Password: password}, InstallerPath: installer, Artifacts: artifacts,
		Role: "node", Transport: transport, VerifyArtifact: acceptArtifact,
		Enroll:             func(context.Context) error { return nil },
		VerifyMeasurements: func(context.Context) error { return nil },
	})
	if err != nil || result.Stage != "complete" || !result.Enrolled || !result.MeasurementsVerified {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if string(password) != string(make([]byte, len(password))) || len(transport.authSeen) == 0 || string(transport.authSeen) != "temporary-password" {
		t.Fatalf("credential lifetime/capture unexpected caller=%q transport=%q", password, transport.authSeen)
	}
	for _, command := range transport.commands {
		if strings.Contains(command, "temporary-password") {
			t.Fatalf("credential leaked into command: %q", command)
		}
	}
}

func TestInstallOverSSHDoesNotUploadOnUnknownHost(t *testing.T) {
	root := t.TempDir()
	installer := filepath.Join(root, "payesh-install")
	if err := os.WriteFile(installer, []byte("installer"), 0o700); err != nil {
		t.Fatal(err)
	}
	key := testHostKey("node.example", 22, 8)
	transport := &fakeSSHTransport{keys: []SSHHostKey{key}}
	artifacts := map[string]string{}
	for _, name := range requiredArtifacts("node") {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(name), 0o700); err != nil {
			t.Fatal(err)
		}
		artifacts[name] = path
	}
	_, err := InstallOverSSH(context.Background(), SSHInstallOptions{
		Endpoint: SSHEndpoint{Host: "node.example", Port: 22, User: "root"},
		Auth:     SSHAuth{PrivateKey: []byte("key")}, InstallerPath: installer, Artifacts: artifacts,
		Role: "node", Transport: transport, VerifyArtifact: acceptArtifact,
	})
	if !errors.Is(err, ErrSSHHostKeyUnknown) || len(transport.uploads) != 0 || len(transport.commands) != 0 {
		t.Fatalf("unknown host err=%v uploads=%v commands=%v", err, transport.uploads, transport.commands)
	}
}

func TestInstallOverSSHRequiresVerifierAndRejectsUnsafeInstallerPaths(t *testing.T) {
	root := t.TempDir()
	installer := filepath.Join(root, "payesh-install")
	if err := os.WriteFile(installer, []byte("installer"), 0o700); err != nil {
		t.Fatal(err)
	}
	key := testHostKey("node.example", 22, 10)
	base := SSHInstallOptions{
		Endpoint:                   SSHEndpoint{Host: "node.example", Port: 22, User: "root"},
		ExpectedHostKeyFingerprint: key.Fingerprint,
		Auth:                       SSHAuth{PrivateKey: []byte("key")}, InstallerPath: installer,
		Role: "node", Transport: &fakeSSHTransport{keys: []SSHHostKey{key}},
	}
	if _, err := InstallOverSSH(context.Background(), base); !errors.Is(err, ErrArtifactVerifierRequired) {
		t.Fatalf("missing verifier err=%v", err)
	}
	base.VerifyArtifact = acceptArtifact
	base.InstallerPath = "payesh-install"
	if _, err := InstallOverSSH(context.Background(), base); !errors.Is(err, ErrUnsafeArtifactPath) {
		t.Fatalf("relative installer err=%v", err)
	}
	dashPath := filepath.Join(root, "-payesh-install")
	if err := os.WriteFile(dashPath, []byte("installer"), 0o700); err != nil {
		t.Fatal(err)
	}
	base.InstallerPath = dashPath
	if _, err := InstallOverSSH(context.Background(), base); !errors.Is(err, ErrUnsafeArtifactPath) {
		t.Fatalf("dash installer err=%v", err)
	}
	symlink := filepath.Join(root, "installer-link")
	if err := os.Symlink(installer, symlink); err != nil {
		t.Fatal(err)
	}
	base.InstallerPath = symlink
	if _, err := InstallOverSSH(context.Background(), base); !errors.Is(err, ErrUnsafeArtifactPath) {
		t.Fatalf("symlink installer err=%v", err)
	}
}

func TestInstallOverSSHRejectsSymlinkRoleArtifact(t *testing.T) {
	root := t.TempDir()
	installer := filepath.Join(root, "payesh-install")
	if err := os.WriteFile(installer, []byte("installer"), 0o700); err != nil {
		t.Fatal(err)
	}
	artifacts := map[string]string{}
	for _, name := range requiredArtifacts("node") {
		path := filepath.Join(root, name)
		if name == "payesh-agent" {
			if err := os.Symlink(installer, path); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(path, []byte(name), 0o700); err != nil {
			t.Fatal(err)
		}
		artifacts[name] = path
	}
	key := testHostKey("node.example", 22, 11)
	_, err := InstallOverSSH(context.Background(), SSHInstallOptions{
		Endpoint:                   SSHEndpoint{Host: "node.example", Port: 22, User: "root"},
		ExpectedHostKeyFingerprint: key.Fingerprint,
		Auth:                       SSHAuth{PrivateKey: []byte("key")}, InstallerPath: installer,
		Artifacts: artifacts, Role: "node", Transport: &fakeSSHTransport{keys: []SSHHostKey{key}},
		VerifyArtifact: acceptArtifact,
	})
	if !errors.Is(err, ErrUnsafeArtifactPath) {
		t.Fatalf("symlink role artifact err=%v", err)
	}
}

func TestSSHInstallFailureDetail(t *testing.T) {
	err := sshStage("scan host key", errors.New("host key scan failed for 89.58.29.206:22 (connection refused)\nextra line"))
	if stage := SSHInstallFailureStage(err); stage != "scan host key" {
		t.Fatalf("unexpected stage: %s", stage)
	}
	detail := SSHInstallFailureDetail(err)
	if !strings.Contains(detail, "connection refused") || !strings.Contains(detail, "target port is closed") {
		t.Fatalf("unexpected detail: got %q", detail)
	}
}

type githubCapableTransport struct {
	fakeSSHTransport
	githubSuccess bool
}

func (g *githubCapableTransport) Run(ctx context.Context, ep SSHEndpoint, kh string, auth SSHAuth, command string, stdin []byte) ([]byte, error) {
	g.commands = append(g.commands, command)
	if strings.Contains(command, "status.txt") {
		if g.githubSuccess {
			return []byte("SUCCESS:0\n"), nil
		}
		return []byte("FALLBACK:master_upload_required\n"), nil
	}
	if strings.Contains(command, "preflight.json") {
		return json.Marshal(g.preflight)
	}
	if strings.Contains(command, " --json ") {
		return json.Marshal(g.preflight)
	}
	return nil, nil
}

func TestInstallOverSSHGithubFirstSucceedsWithoutUpload(t *testing.T) {
	root := t.TempDir()
	installer := filepath.Join(root, "payesh-install")
	_ = os.WriteFile(installer, []byte("installer"), 0o700)
	artifacts := map[string]string{}
	for _, name := range requiredArtifacts("node") {
		path := filepath.Join(root, name)
		_ = os.WriteFile(path, []byte(name), 0o700)
		artifacts[name] = path
	}
	key := testHostKey("node.example", 22, 12)
	transport := &githubCapableTransport{
		fakeSSHTransport: fakeSSHTransport{
			keys:      []SSHHostKey{key},
			preflight: Preflight{Role: "node", Supported: true, Artifacts: requiredArtifacts("node")},
		},
		githubSuccess: true,
	}
	result, err := InstallOverSSH(context.Background(), SSHInstallOptions{
		Endpoint:                   SSHEndpoint{Host: "node.example", Port: 22, User: "root"},
		ExpectedHostKeyFingerprint: key.Fingerprint,
		Auth:                       SSHAuth{PrivateKey: []byte("key")},
		InstallerPath:              installer,
		Artifacts:                  artifacts,
		Role:                       "node",
		Transport:                  transport,
		VerifyArtifact:             acceptArtifact,
		Enroll:                     func(context.Context) error { return nil },
		VerifyMeasurements:         func(context.Context) error { return nil },
	})
	if err != nil || result.Stage != "complete" || !result.Enrolled {
		t.Fatalf("unexpected result: result=%+v err=%v", result, err)
	}
	if len(transport.uploads) != 0 {
		t.Fatalf("expected 0 uploads when GitHub succeeds, got %d: %v", len(transport.uploads), transport.uploads)
	}
}

func TestInstallOverSSHGithubFallbackUploadsFromMaster(t *testing.T) {
	root := t.TempDir()
	installer := filepath.Join(root, "payesh-install")
	_ = os.WriteFile(installer, []byte("installer"), 0o700)
	artifacts := map[string]string{}
	for _, name := range requiredArtifacts("node") {
		path := filepath.Join(root, name)
		_ = os.WriteFile(path, []byte(name), 0o700)
		artifacts[name] = path
	}
	key := testHostKey("node.example", 22, 13)
	transport := &githubCapableTransport{
		fakeSSHTransport: fakeSSHTransport{
			keys:      []SSHHostKey{key},
			preflight: Preflight{Role: "node", Supported: true, Artifacts: requiredArtifacts("node")},
		},
		githubSuccess: false,
	}
	result, err := InstallOverSSH(context.Background(), SSHInstallOptions{
		Endpoint:                   SSHEndpoint{Host: "node.example", Port: 22, User: "root"},
		ExpectedHostKeyFingerprint: key.Fingerprint,
		Auth:                       SSHAuth{PrivateKey: []byte("key")},
		InstallerPath:              installer,
		Artifacts:                  artifacts,
		Role:                       "node",
		Transport:                  transport,
		VerifyArtifact:             acceptArtifact,
		Enroll:                     func(context.Context) error { return nil },
		VerifyMeasurements:         func(context.Context) error { return nil },
	})
	if err != nil || result.Stage != "complete" || !result.Enrolled {
		t.Fatalf("unexpected result: result=%+v err=%v", result, err)
	}
	if len(transport.uploads) == 0 {
		t.Fatalf("expected uploads when GitHub fails and fallback engages")
	}
}

type hubDownloadTransport struct{ fakeSSHTransport }

func (f *hubDownloadTransport) Run(ctx context.Context, ep SSHEndpoint, hosts string, auth SSHAuth, command string, stdin []byte) ([]byte, error) {
	if strings.Contains(command, "# payesh-executable-staging") {
		f.commands = append(f.commands, command)
		return []byte("/var/tmp/payesh-install-fixture"), nil
	}
	if command == "uname -m" {
		f.commands = append(f.commands, command)
		return []byte("aarch64\n"), nil
	}
	if strings.HasPrefix(command, "sh -c ") {
		f.commands = append(f.commands, command)
		return []byte("download_source=hub\n"), nil
	}
	return f.fakeSSHTransport.Run(ctx, ep, hosts, auth, command, stdin)
}

func TestInstallOverSSHUsesHubHTTPWithoutUploadingBinaries(t *testing.T) {
	paths := downloadFixture(t)
	key := testHostKey("node.example", 2222, 4)
	transport := &hubDownloadTransport{fakeSSHTransport: fakeSSHTransport{keys: []SSHHostKey{key}, preflight: Preflight{Role: "node", Supported: true, Architecture: "arm64", Artifacts: requiredArtifacts("node")}}}
	closed := false
	opts := SSHInstallOptions{Endpoint: SSHEndpoint{Host: "node.example", Port: 2222, User: "root"}, ExpectedHostKeyFingerprint: key.Fingerprint, Auth: SSHAuth{Password: []byte("transient")}, InstallerPath: paths["payesh-install"], Artifacts: paths, Role: "node", Transport: transport, VerifyArtifact: acceptArtifact, Enroll: func(context.Context) error { return nil }, VerifyMeasurements: func(context.Context) error { return nil }}
	opts.DownloadArtifacts = func(_ context.Context, arch, role string) (ArtifactDownload, error) {
		if arch != "arm64" || role != "node" {
			t.Fatalf("selection %s %s", arch, role)
		}
		digests := map[string]string{}
		for name, path := range paths {
			digest, err := ArtifactDigest(path, false)
			if err != nil {
				t.Fatal(err)
			}
			digests[name] = digest
		}
		return ArtifactDownload{URL: "https://hub.example/api/v1/install-artifacts/token/bundle.tar.gz", SHA256: strings.Repeat("a", 64), Digests: digests, Close: func() { closed = true }}, nil
	}
	result, err := InstallOverSSH(context.Background(), opts)
	if err != nil || result.Stage != "complete" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(transport.uploads) != 0 {
		t.Fatalf("binary uploads=%v", transport.uploads)
	}
	if !closed {
		t.Fatal("download lease not closed")
	}
}
