package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenRCStartupDropsUserBeforeReadingConfiguration(t *testing.T) {
	for _, transport := range []string{"", "wss://fixture.invalid:9797/node/v1"} {
		t.Run(map[bool]string{true: "remote", false: "local"}[transport != ""], func(t *testing.T) {
			root := t.TempDir()
			write := func(name, body string) string {
				path := filepath.Join(root, name)
				if err := os.WriteFile(path, []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
				return path
			}
			config := write("env", "PAYESH_TRANSPORT_URL='"+transport+"'\n")
			probe := write("payesh", "#!/bin/sh\n[ \"$PAYESH_TEST_SERVICE_USER\" = payesh ] || exit 23\nprintf '%s\\n' \"$*\" > \"$PAYESH_TEST_REGISTRATION\"\n")
			switcher := write("su", "#!/bin/sh\n[ \"$1\" = -s ] && [ \"$2\" = /bin/sh ] && [ \"$3\" = -c ] && [ \"$5\" = payesh ] || exit 24\nPAYESH_TEST_SERVICE_USER=payesh /bin/sh -c \"$4\"\n")
			body, ok := serviceDefinition("openrc", "payesh-agent", "127.0.0.1:0")
			if !ok {
				t.Fatal("missing service")
			}
			body = strings.ReplaceAll(body, "/usr/bin/payesh ", probe+" ")
			body = strings.ReplaceAll(body, "/bin/su", switcher)
			body = strings.ReplaceAll(body, "/etc/payesh/payesh.env", config)
			script := write("service", body)
			record := filepath.Join(root, "registered")
			command := exec.Command("/bin/sh", "-c", ". \"$1\"; start_pre", "test", script)
			command.Env = append(os.Environ(), "PATH="+root+":"+os.Getenv("PATH"), "PAYESH_TRANSPORT_URL=", "PAYESH_TEST_SERVICE_USER=", "PAYESH_TEST_REGISTRATION="+record)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("startup failed: %v %s", err, output)
			}
			_, err := os.Stat(record)
			if transport == "" && err != nil {
				t.Fatal("local startup did not register as service user")
			}
			if transport != "" && !os.IsNotExist(err) {
				t.Fatal("remote startup created local identity")
			}
		})
	}
}

func TestOpenRCAgentDefersAndExportsTransportConfiguration(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "env")
	transport := "wss://fixture.invalid:9797/node/v1"
	if err := os.WriteFile(config, []byte("PAYESH_TRANSPORT_URL='"+transport+"'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(root, "agent")
	if err := os.WriteFile(probe, []byte("#!/bin/sh\nprintf '%s' \"$PAYESH_TRANSPORT_URL\" > \"$PAYESH_TEST_OUTPUT\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	body, _ := serviceDefinition("openrc", "payesh-agent", "127.0.0.1:0")
	body = strings.ReplaceAll(body, "/etc/payesh/payesh.env", config)
	body = strings.ReplaceAll(body, "/usr/bin/payesh-agent", probe)
	// Alpine uses ash with pipefail; bash supplies equivalent shell behavior here.
	body = strings.ReplaceAll(body, "command=\"/bin/sh\"", "command=\"/bin/bash\"")
	script := filepath.Join(root, "service")
	if err := os.WriteFile(script, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "transport")
	command := exec.Command("/bin/bash", "-c", ". \"$1\"; eval \"set -- $command_args\"; \"$command\" \"$@\"", "test", script)
	command.Env = append(os.Environ(), "PAYESH_TRANSPORT_URL=", "PAYESH_TEST_OUTPUT="+output)
	if raw, err := command.CombinedOutput(); err != nil {
		t.Fatalf("agent launch failed: %v %s", err, raw)
	}
	raw, err := os.ReadFile(output)
	if err != nil || string(raw) != transport {
		t.Fatalf("transport env lost: %q %v", raw, err)
	}
}

func TestOpenRCServerExportsInstallerEnvironment(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "env")
	if err := os.WriteFile(config, []byte("PAYESH_LOCAL_TOKEN=fixture-local-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(root, "server")
	if err := os.WriteFile(probe, []byte("#!/bin/sh\nprintf '%s' \"$PAYESH_LOCAL_TOKEN\" > \"$PAYESH_TEST_OUTPUT\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	body, _ := serviceDefinition("openrc", "payesh-server", "127.0.0.1:0")
	body = strings.ReplaceAll(body, "/etc/payesh/payesh.env", config)
	body = strings.ReplaceAll(body, "/usr/bin/payesh-server", probe)
	script := filepath.Join(root, "service")
	if err := os.WriteFile(script, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "token")
	command := exec.Command("/bin/sh", "-c", ". \"$1\"; eval \"set -- $command_args\"; \"$command\" \"$@\"", "test", script)
	command.Env = append(os.Environ(), "PAYESH_LOCAL_TOKEN=", "PAYESH_TEST_OUTPUT="+output)
	if raw, err := command.CombinedOutput(); err != nil {
		t.Fatalf("server launch failed: %v %s", err, raw)
	}
	raw, err := os.ReadFile(output)
	if err != nil || string(raw) != "fixture-local-token" {
		t.Fatalf("server env lost: %q %v", raw, err)
	}
}

func TestOpenRCWrappedServerListenerRecognizedOnRerun(t *testing.T) {
	root, artifacts := installFixture(t, "openrc")
	for _, name := range []string{"payesh-server"} {
		if err := os.WriteFile(filepath.Join(artifacts, name), []byte("server"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(artifacts, "web-assets"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifacts, "web-assets/index.html"), []byte("dashboard"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Install(t.Context(), InstallOptions{Root: root, Role: "standalone", Listen: "127.0.0.1:0", ArtifactDir: artifacts, Verify: acceptArtifact, AccountManager: &testAccountManager{}})
	if err != nil {
		t.Fatal(err)
	}
	if !existingServerListen(root, "standalone", "openrc", "127.0.0.1:0") {
		t.Fatal("generated wrapped listener is not recognized")
	}
	if existingServerListen(root, "standalone", "openrc", "127.0.0.1:1") {
		t.Fatal("unmatched listener was accepted")
	}
}

func TestOpenRCShippedDefinitionsMatchGeneratedServices(t *testing.T) {
	for _, name := range []string{"payesh-agent", "payesh-server"} {
		shipped, err := os.ReadFile(filepath.Join("..", "..", "deploy", "openrc", name))
		if err != nil {
			t.Fatal(err)
		}
		generated, ok := serviceDefinition("openrc", name, "0.0.0.0:8787")
		if !ok || string(shipped) != generated {
			t.Fatalf("shipped %s differs from installer fallback", name)
		}
	}
}
