package install

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Real-kia/payesh/internal/netproxy"
)

type stdinRecordingTransport struct {
	fakeSSHTransport
	stdin map[string]string
}

func (f *stdinRecordingTransport) Run(ctx context.Context, ep SSHEndpoint, hosts string, auth SSHAuth, command string, stdin []byte) ([]byte, error) {
	if f.stdin == nil {
		f.stdin = map[string]string{}
	}
	if stdin != nil {
		f.stdin[command] = string(stdin)
	}
	return f.fakeSSHTransport.Run(ctx, ep, hosts, auth, command, stdin)
}

func TestProxiedInstallUploadsAndConfiguresNodeProxy(t *testing.T) {
	root := t.TempDir()
	installer := filepath.Join(root, "payesh-install")
	_ = os.WriteFile(installer, []byte("installer"), 0o700)
	artifacts := map[string]string{}
	for _, name := range requiredArtifacts("node") {
		path := filepath.Join(root, name)
		_ = os.WriteFile(path, []byte(name), 0o700)
		artifacts[name] = path
	}
	key := testHostKey("10.20.30.40", 22, 21)
	transport := &stdinRecordingTransport{fakeSSHTransport: fakeSSHTransport{keys: []SSHHostKey{key}, preflight: Preflight{Role: "node", Supported: true, Artifacts: requiredArtifacts("node")}}}
	proxy, _ := netproxy.Parse("http://install-job:install-secret@203.0.113.5:9798")
	nodeProxy := "http://node-target:node-secret@203.0.113.5:9798"
	result, err := InstallOverSSH(context.Background(), SSHInstallOptions{
		Endpoint:                   SSHEndpoint{Host: "10.20.30.40", Port: 22, User: "root"},
		ExpectedHostKeyFingerprint: key.Fingerprint,
		Auth:                       SSHAuth{PrivateKey: []byte("key")},
		InstallerPath:              installer, Artifacts: artifacts, Role: "node",
		TransportURL: "wss://hub.example.test:9797/node/v1", NodeIdentityJSON: []byte("fixture identity"), HubTrustPEM: []byte("fixture trust"),
		Transport: transport, VerifyArtifact: acceptArtifact,
		Enroll:             func(context.Context) error { return nil },
		VerifyMeasurements: func(context.Context) error { return nil },
		Proxy:              proxy, NodeProxy: nodeProxy,
	})
	if err != nil || result.Stage != "complete" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, command := range transport.commands {
		if strings.Contains(command, "github_bootstrap.sh") {
			t.Fatal("a proxied install must not ask the target to download from GitHub")
		}
	}
	if len(transport.uploads) == 0 {
		t.Fatal("a proxied install must upload the files over SSH")
	}
	portCheck := false
	for _, command := range transport.commands {
		if strings.Contains(command, "--check-transport") {
			portCheck = strings.Contains(command, "--transport-proxy '"+nodeProxy+"'")
		}
	}
	if !portCheck {
		t.Fatalf("node port check does not use the node proxy: %q", transport.commands)
	}
	envFound := false
	for command, body := range transport.stdin {
		if strings.Contains(command, "payesh.env") {
			envFound = strings.Contains(body, "PAYESH_TRANSPORT_PROXY="+nodeProxy+"\n") && strings.Contains(body, "PAYESH_TRANSPORT_URL=wss://hub.example.test:9797/node/v1\n")
		}
	}
	if !envFound {
		t.Fatalf("node environment lacks the proxy: %v", transport.stdin)
	}
}

func TestOpenSSHProxyCommandKeepsCredentialsOutOfArguments(t *testing.T) {
	proxy, _ := netproxy.Parse("socks5://alice:s3cret@203.0.113.5:1080")
	transport := OpenSSHTransport{Proxy: proxy, BridgeExecutable: "/usr/bin/payesh-server"}
	endpoint := SSHEndpoint{Host: "10.20.30.40", Port: 22, User: "root"}
	want := "ProxyCommand='/usr/bin/payesh-server' ssh-proxy-bridge %h %p"
	for name, args := range map[string][]string{
		"ssh":   transport.sshArgs(endpoint, "/tmp/known", "true"),
		"scp":   transport.scpArgs(endpoint, "/tmp/known", "/tmp/source", "/tmp/destination", false),
		"rsync": transport.rsyncArgs(endpoint, "/tmp/known", "/tmp/source", "/tmp/destination", false),
	} {
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "ssh-proxy-bridge %h %p") || !strings.Contains(joined, "/usr/bin/payesh-server") {
			t.Fatalf("%s lacks the proxy command: %s", name, joined)
		}
		if strings.Contains(joined, "s3cret") {
			t.Fatalf("%s puts the proxy password on the command line", name)
		}
	}
	if args := transport.sshArgs(endpoint, "/tmp/known", "true"); !contains(args, want) {
		t.Fatalf("ssh ProxyCommand = %q", args)
	}
	env := transport.proxyEnv()
	if len(env) != 1 || env[0] != SSHProxyEnv+"=socks5://alice:s3cret@203.0.113.5:1080" {
		t.Fatalf("proxy env = %q", env)
	}
	if direct := (OpenSSHTransport{}); direct.proxyCommand() != "" || direct.proxyEnv() != nil {
		t.Fatal("a direct transport must not set a proxy")
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// directCheckTransport fails or passes the node's direct hub check.
type directCheckTransport struct {
	stdinRecordingTransport
	directWorks bool
}

func (f *directCheckTransport) Run(ctx context.Context, ep SSHEndpoint, hosts string, auth SSHAuth, command string, stdin []byte) ([]byte, error) {
	if strings.Contains(command, "--check-transport") && !strings.Contains(command, "--transport-proxy") && !f.directWorks {
		f.commands = append(f.commands, command)
		return nil, context.DeadlineExceeded
	}
	return f.stdinRecordingTransport.Run(ctx, ep, hosts, auth, command, stdin)
}

func TestAutomaticNodeRoutePrefersDirect(t *testing.T) {
	for _, directWorks := range []bool{true, false} {
		root := t.TempDir()
		installer := filepath.Join(root, "payesh-install")
		_ = os.WriteFile(installer, []byte("installer"), 0o700)
		artifacts := map[string]string{}
		for _, name := range requiredArtifacts("node") {
			path := filepath.Join(root, name)
			_ = os.WriteFile(path, []byte(name), 0o700)
			artifacts[name] = path
		}
		key := testHostKey("10.20.30.40", 22, 22)
		transport := &directCheckTransport{stdinRecordingTransport: stdinRecordingTransport{fakeSSHTransport: fakeSSHTransport{keys: []SSHHostKey{key}, preflight: Preflight{Role: "node", Supported: true, Artifacts: requiredArtifacts("node")}}}, directWorks: directWorks}
		proxy, _ := netproxy.Parse("http://install:secret@203.0.113.5:9798")
		nodeProxy := "http://node:secret@203.0.113.5:9798"
		result, err := InstallOverSSH(context.Background(), SSHInstallOptions{
			Endpoint: SSHEndpoint{Host: "10.20.30.40", Port: 22, User: "root"}, ExpectedHostKeyFingerprint: key.Fingerprint,
			Auth: SSHAuth{Password: []byte("pw")}, InstallerPath: installer, Artifacts: artifacts, Role: "node",
			TransportURL: "wss://hub.example.test:9797/node/v1", NodeIdentityJSON: []byte("identity"), HubTrustPEM: []byte("trust"),
			Transport: transport, VerifyArtifact: acceptArtifact,
			Enroll: func(context.Context) error { return nil }, VerifyMeasurements: func(context.Context) error { return nil },
			Proxy: proxy, NodeProxy: nodeProxy, NodeProxyAuto: true,
		})
		if err != nil {
			t.Fatalf("directWorks=%v: %v", directWorks, err)
		}
		if result.NodeProxied == directWorks {
			t.Fatalf("directWorks=%v but NodeProxied=%v", directWorks, result.NodeProxied)
		}
		// The last staged environment is what applyNodeConfig installs.
		var env string
		for command, body := range transport.stdin {
			if strings.Contains(command, "payesh.env") {
				env = body
			}
		}
		if strings.Contains(env, "PAYESH_TRANSPORT_PROXY") == directWorks {
			t.Fatalf("directWorks=%v staged env: %q", directWorks, env)
		}
	}
}

func TestProxiedInstallUploadsArchitectureBuilds(t *testing.T) {
	root := t.TempDir()
	installer := filepath.Join(root, "payesh-install")
	_ = os.WriteFile(installer, []byte("installer"), 0o700)
	hubArtifacts, amd64 := map[string]string{}, map[string]string{"payesh-install": filepath.Join(root, "payesh-install-linux-amd64")}
	_ = os.WriteFile(amd64["payesh-install"], []byte("amd64 installer"), 0o700)
	for _, name := range requiredArtifacts("node") {
		hubArtifacts[name] = filepath.Join(root, name)
		amd64[name] = filepath.Join(root, name+"-linux-amd64")
		_ = os.WriteFile(hubArtifacts[name], []byte(name), 0o700)
		_ = os.WriteFile(amd64[name], []byte(name+" amd64"), 0o700)
	}
	key := testHostKey("10.20.30.40", 22, 23)
	run := func(selectArtifacts func(string, string) (map[string]string, error)) (*fakeSSHTransport, error) {
		transport := &fakeSSHTransport{keys: []SSHHostKey{key}, preflight: Preflight{Role: "node", Supported: true, Artifacts: requiredArtifacts("node")}}
		proxy, _ := netproxy.Parse("http://u:p@203.0.113.5:9798")
		_, err := InstallOverSSH(context.Background(), SSHInstallOptions{
			Endpoint: SSHEndpoint{Host: "10.20.30.40", Port: 22, User: "root"}, ExpectedHostKeyFingerprint: key.Fingerprint,
			Auth: SSHAuth{Password: []byte("pw")}, InstallerPath: installer, Artifacts: hubArtifacts, Role: "node",
			TransportURL: "wss://hub.example.test:9797/node/v1", NodeIdentityJSON: []byte("identity"), HubTrustPEM: []byte("trust"),
			Transport: transport, VerifyArtifact: acceptArtifact,
			Enroll: func(context.Context) error { return nil }, VerifyMeasurements: func(context.Context) error { return nil },
			Proxy: proxy, SelectArtifacts: selectArtifacts,
		})
		return transport, err
	}
	var gotArch string
	transport, err := run(func(arch, role string) (map[string]string, error) { gotArch = arch; return amd64, nil })
	if err != nil {
		t.Fatal(err)
	}
	if gotArch != "amd64" {
		t.Fatalf("selected for %q", gotArch)
	}
	for _, upload := range transport.uploads {
		if !strings.Contains(upload, "-linux-amd64=>") {
			t.Fatalf("uploaded a non-amd64 file: %s", upload)
		}
	}
	if _, err := run(func(string, string) (map[string]string, error) {
		return nil, os.ErrNotExist
	}); err == nil || SSHInstallFailureStage(err) != "select installation files" {
		t.Fatalf("missing build: %v", err)
	}
}
