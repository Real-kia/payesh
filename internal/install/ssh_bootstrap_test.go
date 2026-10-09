package install

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type corruptUploadTransport struct{ githubCapableTransport }

func (r *corruptUploadTransport) Run(ctx context.Context, ep SSHEndpoint, hosts string, auth SSHAuth, command string, stdin []byte) ([]byte, error) {
	if strings.Contains(command, "# payesh-verify-upload") {
		r.commands = append(r.commands, command)
		return nil, errors.New("staged artifact differs from trusted pin")
	}
	return r.githubCapableTransport.Run(ctx, ep, hosts, auth, command, stdin)
}

func TestLegacySSHUploadVerificationPrecedesExecution(t *testing.T) {
	paths := downloadFixture(t)
	key := testHostKey("node.example", 22, 24)
	remote := &corruptUploadTransport{githubCapableTransport: githubCapableTransport{fakeSSHTransport: fakeSSHTransport{keys: []SSHHostKey{key}, preflight: Preflight{Role: "node", Supported: true, Artifacts: requiredArtifacts("node")}}}}
	_, err := InstallOverSSH(context.Background(), SSHInstallOptions{Endpoint: SSHEndpoint{Host: "node.example", Port: 22, User: "root"}, ExpectedHostKeyFingerprint: key.Fingerprint, Auth: SSHAuth{PrivateKey: []byte("fixture")}, InstallerPath: paths["payesh-install"], Artifacts: paths, Role: "node", Transport: remote, VerifyArtifact: acceptArtifact, TransportURL: "wss://hub.example.test:9797/node/v1", NodeIdentityJSON: []byte("fixture identity"), HubTrustPEM: []byte("fixture trust"), Enroll: func(context.Context) error { return nil }, VerifyMeasurements: func(context.Context) error { return nil }})
	if err == nil {
		t.Fatal("corrupt uploaded executable reached installation")
	}
	for _, c := range remote.commands {
		if !strings.HasPrefix(c, "cat ") && (strings.Contains(c, " --json --role ") || strings.Contains(c, " --install --role ") || strings.Contains(c, " --check-transport ")) {
			t.Fatal("installer executed before uploaded pins checked")
		}
	}
}

func TestLegacySSHFullRolesUseVerifiedUpload(t *testing.T) {
	for _, role := range []string{"hub", "standalone"} {
		t.Run(role, func(t *testing.T) {
			root := t.TempDir()
			paths := map[string]string{}
			for _, name := range append([]string{"payesh-install"}, requiredArtifacts(role)...) {
				p := filepath.Join(root, name)
				if name == "web-assets" {
					if e := os.Mkdir(p, 0700); e != nil {
						t.Fatal(e)
					}
					if e := os.WriteFile(filepath.Join(p, "index.html"), []byte("verified web content"), 0600); e != nil {
						t.Fatal(e)
					}
				} else if e := os.WriteFile(p, []byte(name), 0700); e != nil {
					t.Fatal(e)
				}
				paths[name] = p
			}
			key := testHostKey("node.example", 22, 25)
			remote := &githubCapableTransport{fakeSSHTransport: fakeSSHTransport{keys: []SSHHostKey{key}, preflight: Preflight{Role: role, Supported: true, Artifacts: requiredArtifacts(role)}}, githubSuccess: true}
			_, err := InstallOverSSH(context.Background(), SSHInstallOptions{Endpoint: SSHEndpoint{Host: "node.example", Port: 22, User: "root"}, ExpectedHostKeyFingerprint: key.Fingerprint, Auth: SSHAuth{PrivateKey: []byte("fixture")}, InstallerPath: paths["payesh-install"], Artifacts: paths, Role: role, Transport: remote, VerifyArtifact: acceptArtifact, Enroll: func(context.Context) error { return nil }, VerifyMeasurements: func(context.Context) error { return nil }})
			if err != nil {
				t.Fatal(err)
			}
			if len(remote.uploads) == 0 {
				t.Fatal("directory-bearing role used file-only GitHub bootstrap")
			}
			digest, err := ArtifactDigest(paths["web-assets"], true)
			if err != nil {
				t.Fatal(err)
			}
			copied, pinned := false, false
			for _, u := range remote.uploads {
				if strings.HasPrefix(u, paths["web-assets"]+"=>") && strings.HasSuffix(u, ":recursive") {
					copied = true
				}
			}
			for _, c := range remote.commands {
				if strings.Contains(c, " --install --role ") && strings.Contains(c, "web-assets="+digest) {
					pinned = true
				}
			}
			if !copied || !pinned {
				t.Fatal("nonempty web directory not uploaded recursively with trusted tree pin")
			}
		})
	}
}

// Execute the actual SSH-generated shell with prefilled download files. No
// network or installed service paths are used; cli-only skips node setup.
func TestLegacySSHBootstrapAuthenticatesBeforeInstallerExecution(t *testing.T) {
	for _, tamper := range []string{"payesh-install", "payesh", "payesh-agent", ""} {
		t.Run("tamper="+tamper, func(t *testing.T) {
			root := t.TempDir()
			role := "cli-only"
			if tamper == "payesh-agent" {
				role = "node"
			}
			installerBody := []byte("#!/bin/sh\nprintf invoked >> \"$TEST_INSTALLER_MARKER\"\nif [ \"$1\" = --json ]; then printf '{}'; fi\nexit 0\n")
			installer := filepath.Join(root, "trusted-installer")
			if err := os.WriteFile(installer, installerBody, 0700); err != nil {
				t.Fatal(err)
			}
			paths := map[string]string{}
			for _, name := range requiredArtifacts(role) {
				path := filepath.Join(root, "trusted-"+name)
				if err := os.WriteFile(path, []byte("trusted "+name), 0700); err != nil {
					t.Fatal(err)
				}
				paths[name] = path
			}
			key := testHostKey("node.example", 22, 22)
			remote := &githubCapableTransport{fakeSSHTransport: fakeSSHTransport{keys: []SSHHostKey{key}, preflight: Preflight{Role: role, Supported: true}}, githubSuccess: true}
			_, err := InstallOverSSH(context.Background(), SSHInstallOptions{Endpoint: SSHEndpoint{Host: "node.example", Port: 22, User: "root"}, ExpectedHostKeyFingerprint: key.Fingerprint, Auth: SSHAuth{PrivateKey: []byte("fixture")}, InstallerPath: installer, Artifacts: paths, Role: role, Transport: remote, VerifyArtifact: acceptArtifact, TransportURL: "wss://hub.example.test:9797/node/v1", NodeIdentityJSON: []byte("fixture"), HubTrustPEM: []byte("fixture"), Enroll: func(context.Context) error { return nil }, VerifyMeasurements: func(context.Context) error { return nil }})
			if err != nil {
				t.Fatal(err)
			}
			var script string
			for _, command := range remote.commands {
				if strings.HasPrefix(command, "cat << 'EOF' > ") && strings.Contains(command, "github_bootstrap.sh") {
					start := strings.IndexByte(command, '\n') + 1
					end := strings.LastIndex(command, "\nEOF\n")
					if start <= 0 || end < start {
						t.Fatal("invalid script handoff")
					}
					script = command[start:end]
				}
			}
			if script == "" {
				t.Fatal("bootstrap not generated")
			}
			stage := filepath.Join(root, "downloaded")
			if err := os.MkdirAll(filepath.Join(stage, "payesh-repo"), 0700); err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(script, "\n") {
				if strings.HasPrefix(line, "DIR='") {
					originalDir := strings.TrimSuffix(strings.TrimPrefix(line, "DIR='"), "'")
					script = strings.ReplaceAll(script, originalDir, stage)
					break
				}
			}
			for name, path := range paths {
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if name == tamper {
					body = []byte("altered artifact")
					if name == "payesh-agent" {
						body = []byte("#!/bin/sh\nprintf invoked >> \"$TEST_PROBE_MARKER\"\nexit 1\n")
					}
				}
				if err := os.WriteFile(filepath.Join(stage, name), body, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if tamper == "payesh-install" {
				installerBody = append(installerBody, []byte("# altered\n")...)
			}
			if err := os.WriteFile(filepath.Join(stage, "payesh-install"), installerBody, 0700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, "executed")
			probeMarker := filepath.Join(root, "probe-executed")
			command := exec.Command("sh", "-c", script)
			command.Env = append(os.Environ(), "TEST_INSTALLER_MARKER="+marker, "TEST_PROBE_MARKER="+probeMarker)
			output, runErr := command.CombinedOutput()
			_, markerErr := os.Stat(marker)
			if tamper != "" {
				if _, err := os.Stat(probeMarker); err == nil {
					t.Fatal("unverified node agent executed transport probe")
				}
				if markerErr == nil {
					t.Fatal("downloaded installer executed before trusted artifact verification")
				}
				if runErr == nil {
					t.Fatalf("altered artifact accepted: %s", output)
				}
				status, err := os.ReadFile(filepath.Join(stage, "status.txt"))
				if err != nil || strings.TrimSpace(string(status)) != "FAILED:artifact_verification" {
					t.Fatalf("refused for wrong reason: %q %v", status, err)
				}
			} else if runErr != nil || markerErr != nil {
				t.Fatalf("trusted install rejected: %v %v %s", runErr, markerErr, output)
			}
		})
	}
}

// A failed observation is not evidence that the asynchronous producer stopped.
type uncertainBootstrapTransport struct {
	githubCapableTransport
	mode string
}

func (f *uncertainBootstrapTransport) Run(ctx context.Context, ep SSHEndpoint, hosts string, auth SSHAuth, command string, stdin []byte) ([]byte, error) {
	if strings.HasPrefix(command, "cat << 'EOF' > ") && strings.Contains(command, "github_bootstrap.sh") && f.mode == "write-error" {
		f.commands = append(f.commands, command)
		return nil, errors.New("script write failed")
	}
	if strings.Contains(command, "preflight.json") && f.mode == "success-read-error" {
		f.commands = append(f.commands, command)
		return nil, errors.New("preflight read failed")
	}
	if strings.Contains(command, "nohup sh") && f.mode == "launch-error" {
		f.commands = append(f.commands, command)
		return nil, errors.New("launch acknowledgement lost")
	}
	if strings.HasPrefix(command, "cat ") && strings.Contains(command, "status.txt") {
		f.commands = append(f.commands, command)
		switch f.mode {
		case "running-status":
			return []byte("RUNNING:github_fetch"), nil
		case "status-error":
			return nil, errors.New("status acknowledgement lost")
		case "empty-status":
			return nil, nil
		case "failed-status":
			return []byte("FAILED:artifact_verification"), nil
		case "success-read-error", "invalid-success":
			return []byte("SUCCESS:0"), nil
		case "unknown-fallback":
			return []byte("FALLBACK:unknown"), nil
		}
	}
	return f.githubCapableTransport.Run(ctx, ep, hosts, auth, command, stdin)
}
func TestSSHUncertainBootstrapNeverUploadsOrExecutesFallback(t *testing.T) {
	for _, mode := range []string{"write-error", "launch-error", "status-error", "empty-status", "failed-status", "invalid-success", "success-read-error", "unknown-fallback"} {
		t.Run(mode, func(t *testing.T) {
			paths := downloadFixture(t)
			key := testHostKey("node.example", 22, 22)
			remote := &uncertainBootstrapTransport{mode: mode, githubCapableTransport: githubCapableTransport{fakeSSHTransport: fakeSSHTransport{keys: []SSHHostKey{key}}}}
			_, err := InstallOverSSH(context.Background(), SSHInstallOptions{Endpoint: SSHEndpoint{Host: "node.example", Port: 22, User: "root"}, ExpectedHostKeyFingerprint: key.Fingerprint, Auth: SSHAuth{PrivateKey: []byte("fixture")}, InstallerPath: paths["payesh-install"], Artifacts: paths, Role: "cli-only", Transport: remote, VerifyArtifact: acceptArtifact})
			if err == nil {
				t.Fatal("uncertain bootstrap accepted")
			}
			if len(remote.uploads) != 0 {
				t.Fatalf("fallback uploads while producer uncertain: %v", remote.uploads)
			}
			for _, command := range remote.commands {
				if strings.Contains(command, "github_bootstrap.sh") {
					continue
				}
				if strings.Contains(command, "# payesh-verify-upload") || strings.Contains(command, " --json --role ") || strings.Contains(command, " --install --role ") || strings.Contains(command, " --check-transport ") {
					t.Fatalf("fallback executed while producer uncertain: %s", command)
				}
			}
		})
	}
}

func TestSSHBootstrapContextDeadlineNeverUploadsFallback(t *testing.T) {
	paths := downloadFixture(t)
	key := testHostKey("node.example", 22, 24)
	remote := &uncertainBootstrapTransport{mode: "running-status", githubCapableTransport: githubCapableTransport{fakeSSHTransport: fakeSSHTransport{keys: []SSHHostKey{key}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := InstallOverSSH(ctx, SSHInstallOptions{Endpoint: SSHEndpoint{Host: "node.example", Port: 22, User: "root"}, ExpectedHostKeyFingerprint: key.Fingerprint, Auth: SSHAuth{PrivateKey: []byte("fixture")}, InstallerPath: paths["payesh-install"], Artifacts: paths, Role: "cli-only", Transport: remote, VerifyArtifact: acceptArtifact})
	if !errors.Is(err, context.DeadlineExceeded) || len(remote.uploads) != 0 {
		t.Fatalf("deadline fallback: uploads=%v err=%v", remote.uploads, err)
	}
}

type schemaRefusingSSHTransport struct{ githubCapableTransport }

func (f *schemaRefusingSSHTransport) Run(ctx context.Context, ep SSHEndpoint, hosts string, auth SSHAuth, command string, stdin []byte) ([]byte, error) {
	if strings.Contains(command, " --check-schema") && !strings.Contains(command, "github_bootstrap.sh") {
		f.commands = append(f.commands, command)
		return nil, errors.New("candidate installer lacks schema compatibility")
	}
	return f.githubCapableTransport.Run(ctx, ep, hosts, auth, command, stdin)
}
func TestSSHCandidateSchemaRefusalPrecedesInstallerPreflightAndActivation(t *testing.T) {
	paths := downloadFixture(t)
	key := testHostKey("node.example", 22, 25)
	remote := &schemaRefusingSSHTransport{githubCapableTransport: githubCapableTransport{fakeSSHTransport: fakeSSHTransport{keys: []SSHHostKey{key}, preflight: Preflight{Role: "node", Supported: true, Artifacts: requiredArtifacts("node")}}}}
	_, err := InstallOverSSH(t.Context(), SSHInstallOptions{Endpoint: SSHEndpoint{Host: "node.example", Port: 22, User: "root"}, ExpectedHostKeyFingerprint: key.Fingerprint, Auth: SSHAuth{PrivateKey: []byte("fixture")}, InstallerPath: paths["payesh-install"], Artifacts: paths, Role: "node", Transport: remote, VerifyArtifact: acceptArtifact, TransportURL: "wss://hub.example.test:9797/node/v1", NodeIdentityJSON: []byte("fixture"), HubTrustPEM: []byte("fixture"), Enroll: func(context.Context) error { return nil }, VerifyMeasurements: func(context.Context) error { return nil }})
	if err == nil || SSHInstallFailureStage(err) != "candidate database schema" {
		t.Fatalf("SSH schema refusal skipped: %v", err)
	}
	for _, command := range remote.commands {
		if strings.Contains(command, "github_bootstrap.sh") {
			continue
		}
		if strings.Contains(command, " --json --role ") || strings.Contains(command, " --install --role ") {
			t.Fatal("incompatible candidate reached installer preflight/activation")
		}
	}
}
