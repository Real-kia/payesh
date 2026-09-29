package install

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testAccountManager struct{ calls int }

func acceptArtifact(string, string) error { return nil }

func (m *testAccountManager) Ensure(context.Context, string, string, string) (Account, error) {
	m.calls++
	return Account{Name: "payesh", Group: "payesh", UID: -1, GID: -1}, nil
}

type testServiceManager struct {
	root, init string
	services   []string
	start      bool
}

func (m *testServiceManager) Apply(_ context.Context, root, init string, services []string, start bool) error {
	m.root, m.init, m.services, m.start = root, init, append([]string(nil), services...), start
	return nil
}

func installFixture(t *testing.T, init string) (string, string) {
	t.Helper()
	root := t.TempDir()
	for _, path := range []string{"etc", "run/systemd/system", "usr/bin"} {
		if init == "openrc" && path == "run/systemd/system" {
			continue
		}
		if err := os.MkdirAll(filepath.Join(root, path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if init == "openrc" {
		if err := os.MkdirAll(filepath.Join(root, "sbin"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "sbin/openrc"), []byte("fixture"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "etc/os-release"), []byte("ID=debian\nVERSION_ID=12\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	artifactDir := t.TempDir()
	for _, name := range []string{"payesh-agent", "payesh-privd", "payesh"} {
		if err := os.WriteFile(filepath.Join(artifactDir, name), []byte(name+"-binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root, artifactDir
}

func TestInstallNodeIsRoleSelectiveAndIdempotent(t *testing.T) {
	root, artifactDir := installFixture(t, "systemd")
	identity := filepath.Join(root, "var/lib/payesh/server-id")
	if err := os.MkdirAll(filepath.Dir(identity), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(identity, []byte("stable-node-id\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	accounts := &testAccountManager{}
	services := &testServiceManager{}
	opts := InstallOptions{Root: root, Role: "node", ArtifactDir: artifactDir, Verify: acceptArtifact, AccountManager: accounts, ServiceManager: services}
	first, err := Install(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(first.Installed, ","), "payesh-agent,payesh-privd,payesh"; got != want {
		t.Fatalf("installed=%q, want %q", got, want)
	}
	if strings.Join(first.Services, ",") != "payesh-agent" || services.start {
		t.Fatalf("unexpected services: %+v", first)
	}
	if _, err := os.Stat(filepath.Join(root, "usr/bin/payesh-server")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("node installation created server artifact: %v", err)
	}
	if got, err := os.ReadFile(identity); err != nil || string(got) != "stable-node-id\n" {
		t.Fatalf("identity changed: %q, %v", got, err)
	}
	for _, path := range []string{"var/lib/payesh", "etc/payesh", "var/log/payesh"} {
		info, err := os.Stat(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o750 {
			t.Fatalf("%s mode=%o, want 750", path, info.Mode().Perm())
		}
	}
	second, err := Install(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Resumed || accounts.calls != 2 {
		t.Fatalf("retry was not reported as resumed: result=%+v accountCalls=%d", second, accounts.calls)
	}
}

func TestInstallKeepsInstallerForRepairAndUninstall(t *testing.T) {
	root, artifactDir := installFixture(t, "systemd")
	installerPath := filepath.Join(t.TempDir(), "payesh-install")
	if err := os.WriteFile(installerPath, []byte("installer-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := Install(context.Background(), InstallOptions{
		Root: root, Role: "node", ArtifactDir: artifactDir, InstallerPath: installerPath,
		Verify: acceptArtifact, AccountManager: &testAccountManager{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Installed[len(result.Installed)-1]; got != "payesh-install" {
		t.Fatalf("last installed artifact=%q", got)
	}
	installed, err := os.ReadFile(filepath.Join(root, "usr/bin/payesh-install"))
	if err != nil || string(installed) != "installer-binary" {
		t.Fatalf("installed repair tool=%q err=%v", installed, err)
	}
}

func TestInstallUpgradeAllowsItsExistingListenAddress(t *testing.T) {
	root, artifactDir := installFixture(t, "systemd")
	for _, name := range []string{"payesh-server"} {
		if err := os.WriteFile(filepath.Join(artifactDir, name), []byte(name+"-binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(artifactDir, "web-assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "web-assets/index.html"), []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	opts := InstallOptions{Root: root, Role: "hub", Listen: address, ArtifactDir: artifactDir, Verify: acceptArtifact, AccountManager: &testAccountManager{}}
	if _, err := Install(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	listener, err = net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	p, err := Check(root, "hub", address)
	if err != nil || !p.Supported {
		t.Fatalf("rerun preflight rejected existing listener: %+v err=%v", p, err)
	}
	other, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	p, err = Check(root, "hub", other.Addr().String())
	if err != nil || p.Supported {
		t.Fatalf("unrelated occupied address accepted: %+v err=%v", p, err)
	}
	p, err = Check(root, "standalone", address)
	if err != nil || p.Supported {
		t.Fatalf("different role accepted: %+v err=%v", p, err)
	}
	if _, err := Install(context.Background(), opts); err != nil {
		t.Fatalf("upgrade rejected the existing Payesh listener: %v", err)
	}
}

func TestInstallPartialArtifactFailureCanRetry(t *testing.T) {
	root, artifactDir := installFixture(t, "systemd")
	if err := os.Remove(filepath.Join(artifactDir, "payesh")); err != nil {
		t.Fatal(err)
	}
	opts := InstallOptions{Root: root, Role: "node", ArtifactDir: artifactDir, Verify: acceptArtifact, AccountManager: &testAccountManager{}}
	result, err := Install(context.Background(), opts)
	if !errors.Is(err, ErrMissingArtifact) || len(result.Installed) != 2 {
		t.Fatalf("partial result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(filepath.Join(root, "usr/bin/payesh-agent")); err != nil {
		t.Fatalf("first artifact was not retained: %v", err)
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "payesh"), []byte("payesh-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	result, err = Install(context.Background(), opts)
	if err != nil || !result.Resumed {
		t.Fatalf("retry result=%+v err=%v", result, err)
	}
}

func TestInstallReplacesExistingWebAssets(t *testing.T) {
	root, artifactDir := installFixture(t, "systemd")
	if err := os.WriteFile(filepath.Join(artifactDir, "payesh-server"), []byte("server-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	web := filepath.Join(artifactDir, "web-assets")
	if err := os.WriteFile(filepath.Join(artifactDir, "payesh-server"), []byte("server-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	asset := filepath.Join(web, "index.html")
	if err := os.WriteFile(asset, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := InstallOptions{Root: root, Role: "standalone", Listen: "127.0.0.1:0", ArtifactDir: artifactDir, Verify: acceptArtifact}
	if _, err := Install(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(asset, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(filepath.Join(root, "usr/share/payesh/web-assets/index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if string(installed) != "second" {
		t.Fatalf("installed web asset = %q", installed)
	}
}

func TestStandaloneInstallGeneratesOwnerCredentials(t *testing.T) {
	root, artifactDir := installFixture(t, "systemd")
	if err := os.WriteFile(filepath.Join(artifactDir, "payesh-server"), []byte("server-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	web := filepath.Join(artifactDir, "web-assets")
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), InstallOptions{Root: root, Role: "standalone", Listen: "127.0.0.1:0", ArtifactDir: artifactDir, Verify: acceptArtifact}); err != nil {
		t.Fatal(err)
	}
	credentials, err := os.ReadFile(filepath.Join(root, "etc/payesh/owner-credentials"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(credentials), "username: owner_") || !strings.Contains(string(credentials), "password: ") {
		t.Fatalf("invalid generated credentials: %q", credentials)
	}
	env, err := os.ReadFile(filepath.Join(root, "etc/payesh/payesh.env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), "PAYESH_OWNER_USERNAME=") || !strings.Contains(string(env), "PAYESH_OWNER_PASSWORD=") || !strings.Contains(string(env), "PAYESH_BOOTSTRAP_SECRET=") {
		t.Fatalf("missing generated owner environment: %q", env)
	}
}

func TestStandaloneInstallPreservesExistingOwnerEnvironment(t *testing.T) {
	root, artifactDir := installFixture(t, "systemd")
	if err := os.WriteFile(filepath.Join(artifactDir, "payesh-server"), []byte("server-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	web := filepath.Join(artifactDir, "web-assets")
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "etc/payesh")
	if err := os.MkdirAll(config, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "payesh.env"), []byte("PAYESH_LOCAL_TOKEN=operator-managed\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), InstallOptions{Root: root, Role: "standalone", Listen: "127.0.0.1:0", ArtifactDir: artifactDir, Verify: acceptArtifact}); err != nil {
		t.Fatal(err)
	}
	env, err := os.ReadFile(filepath.Join(config, "payesh.env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(env) != "PAYESH_LOCAL_TOKEN=operator-managed\n" {
		t.Fatalf("operator environment was changed: %q", env)
	}
	if _, err := os.Stat(filepath.Join(config, "owner-credentials")); !os.IsNotExist(err) {
		t.Fatalf("unexpected generated credentials: %v", err)
	}
}

func TestInstallUsesRequestedListenInServiceDefinition(t *testing.T) {
	for _, init := range []string{"systemd", "openrc"} {
		t.Run(init, func(t *testing.T) {
			root, artifactDir := installFixture(t, init)
			server := filepath.Join(artifactDir, "payesh-server")
			if err := os.WriteFile(server, []byte("server-binary"), 0o755); err != nil {
				t.Fatal(err)
			}
			web := filepath.Join(artifactDir, "web-assets")
			if err := os.MkdirAll(web, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("ok"), 0o644); err != nil {
				t.Fatal(err)
			}
			listen := "127.0.0.1:0"
			if _, err := Install(context.Background(), InstallOptions{Root: root, Role: "standalone", Listen: listen, ArtifactDir: artifactDir, Verify: acceptArtifact}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "etc/systemd/system/payesh-server.service")
			if init == "openrc" {
				path = filepath.Join(root, "etc/init.d/payesh-server")
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), "-listen="+listen) {
				t.Fatalf("service definition does not preserve requested listener: %s", body)
			}
		})
	}
}

func TestInstallOpenRCWritesExecutableDefinitionAndCanStartThroughInjectedManager(t *testing.T) {
	root, artifactDir := installFixture(t, "openrc")
	services := &testServiceManager{}
	result, err := Install(context.Background(), InstallOptions{
		Root: root, Role: "node", ArtifactDir: artifactDir, Verify: acceptArtifact, Start: true,
		AccountManager: &testAccountManager{}, ServiceManager: services,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "etc/init.d/payesh-agent")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("OpenRC mode=%o, want 755", info.Mode().Perm())
	}
	if !result.Started || services.init != "openrc" || !services.start || len(services.services) != 1 {
		t.Fatalf("service activation not delegated: result=%+v manager=%+v", result, services)
	}
}

func TestInstallRejectsUnsupportedTargetBeforeAccountOrFilesystemChanges(t *testing.T) {
	root, artifactDir := installFixture(t, "systemd")
	if err := os.WriteFile(filepath.Join(root, "etc/os-release"), []byte("ID=debian\nVERSION_ID=11\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	accounts := &testAccountManager{}
	_, err := Install(context.Background(), InstallOptions{Root: root, Role: "node", ArtifactDir: artifactDir, AccountManager: accounts})
	var unsupported *UnsupportedError
	if !errors.As(err, &unsupported) || !errors.Is(err, ErrUnsupported) || accounts.calls != 0 {
		t.Fatalf("err=%v accountCalls=%d", err, accounts.calls)
	}
	if _, statErr := os.Stat(filepath.Join(root, "var/lib/payesh")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unsupported install modified target: %v", statErr)
	}
}

func TestInstallRejectsMissingArtifactVerifierBeforeMutation(t *testing.T) {
	root, artifactDir := installFixture(t, "systemd")
	accounts := &testAccountManager{}
	result, err := Install(context.Background(), InstallOptions{Root: root, Role: "node", ArtifactDir: artifactDir, AccountManager: accounts})
	if !errors.Is(err, ErrArtifactVerifierRequired) || accounts.calls != 0 {
		t.Fatalf("result=%+v err=%v accountCalls=%d", result, err, accounts.calls)
	}
	if _, statErr := os.Stat(filepath.Join(root, "var/lib/payesh")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("missing verifier modified target: %v", statErr)
	}
}

func TestInstallRejectsSymlinkArtifactSource(t *testing.T) {
	root, artifactDir := installFixture(t, "systemd")
	target := filepath.Join(t.TempDir(), "payesh-agent-real")
	if err := os.WriteFile(target, []byte("agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(artifactDir, "payesh-agent")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(artifactDir, "payesh-agent")); err != nil {
		t.Fatal(err)
	}
	_, err := Install(context.Background(), InstallOptions{Root: root, Role: "node", ArtifactDir: artifactDir, Verify: acceptArtifact, AccountManager: &testAccountManager{}})
	if !errors.Is(err, ErrUnsafeArtifactPath) {
		t.Fatalf("symlink artifact err=%v", err)
	}
}

type recordingInstallRunner struct{ calls []string }

func (r *recordingInstallRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	return nil, nil
}

func TestServiceActivationRestartsUpdatedBinaries(t *testing.T) {
	for _, init := range []string{"systemd", "openrc"} {
		t.Run(init, func(t *testing.T) {
			runner := &recordingInstallRunner{}
			manager := commandServiceManager{runner: runner}
			services := []string{"payesh-agent", "payesh-server"}
			if err := manager.Apply(context.Background(), "/", init, services, true); err != nil {
				t.Fatal(err)
			}
			calls := strings.Join(runner.calls, "\n")
			for _, service := range services {
				restart := "systemctl restart " + service
				health := "systemctl is-active --quiet " + service
				if init == "openrc" {
					restart = "rc-service " + service + " restart"
					health = "rc-service " + service + " status"
				}
				if !strings.Contains(calls, restart) || !strings.Contains(calls, health) {
					t.Fatalf("missing restart or health check: %s", calls)
				}
			}
		})
	}
}

func TestUpdatePreservesServiceSettingsAndHTTPS(t *testing.T) {
	for _, init := range []string{"systemd", "openrc"} {
		t.Run(init, func(t *testing.T) {
			root, artifacts := installFixture(t, init)
			os.WriteFile(filepath.Join(artifacts, "payesh-server"), []byte("server-v1"), 0755)
			os.Mkdir(filepath.Join(artifacts, "web-assets"), 0755)
			os.WriteFile(filepath.Join(artifacts, "web-assets/index.html"), []byte("dashboard"), 0644)
			opts := InstallOptions{Root: root, Role: "hub", Listen: "127.0.0.1:0", ArtifactDir: artifacts, Verify: acceptArtifact, AccountManager: &testAccountManager{}}
			if _, err := Install(context.Background(), opts); err != nil {
				t.Fatal(err)
			}
			serverUnit := servicePath(root, init, "payesh-server")
			unit, err := os.ReadFile(serverUnit)
			if err != nil {
				t.Fatal(err)
			}
			unit = append(unit, []byte("\n# Operator HTTPS service configuration\n")...)
			if err := os.WriteFile(serverUnit, unit, 0644); err != nil {
				t.Fatal(err)
			}
			tls := filepath.Join(root, "var/lib/payesh/tls")
			if err := os.MkdirAll(tls, 0700); err != nil {
				t.Fatal(err)
			}
			retained := map[string]string{"config.json": `{"domain":"panel.example.com"}`, "cert.pem": "existing-cert", "key.pem": "existing-key"}
			for name, content := range retained {
				if err := os.WriteFile(filepath.Join(tls, name), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			env, _ := os.ReadFile(filepath.Join(root, "etc/payesh/payesh.env"))
			credentials, _ := os.ReadFile(filepath.Join(root, "etc/payesh/owner-credentials"))
			opts.Listen = ""
			if got := installedServerListen(root, init); got != "127.0.0.1:0" {
				t.Fatalf("saved listen=%q", got)
			}
			if err := os.WriteFile(filepath.Join(artifacts, "payesh-server"), []byte("server-v2"), 0755); err != nil {
				t.Fatal(err)
			}
			result, err := Install(context.Background(), opts)
			if err != nil || !result.Resumed {
				t.Fatalf("update=%+v err=%v", result, err)
			}
			for path, want := range map[string]string{serverUnit: string(unit), filepath.Join(root, "etc/payesh/payesh.env"): string(env), filepath.Join(root, "etc/payesh/owner-credentials"): string(credentials), filepath.Join(root, "usr/bin/payesh-server"): "server-v2"} {
				got, err := os.ReadFile(path)
				if err != nil || string(got) != want {
					t.Fatalf("%s changed unexpectedly: err=%v", path, err)
				}
			}
			for name, want := range retained {
				got, err := os.ReadFile(filepath.Join(tls, name))
				if err != nil || string(got) != want {
					t.Fatalf("TLS %s changed: %v", name, err)
				}
			}
		})
	}
}
