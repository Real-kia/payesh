package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHubBootstrapRoutesDownloadsAndClosesSSHRelayOnFailure(t *testing.T) {
	for _, relay := range []bool{false, true} {
		name := "proxy"
		if relay {
			name = "ssh-relay"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range map[string]string{
				"uname": "case \"$1\" in -s) echo Linux;; -m) echo x86_64;; esac",
				"id":    "echo 0",
				"ssh":   "printf '%s\\n' \"$*\" >> \"$RELAY_TEST_LOG\"",
				"curl":  "printf '%s\\n' \"proxy=$https_proxy bypass=$no_proxy $*\" >> \"$RELAY_TEST_LOG\"; exit 22",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"../../install.sh", "--role", "hub", "--release-mode", "preview", "--version", "1.2.3"}
			if relay {
				args = append(args, "--relay-ssh", "operator@relay.example.test", "--relay-ssh-port", "2222")
			} else {
				args = append(args, "--proxy-url", "http://proxy.example.test:8080")
			}
			cmd := exec.Command("sh", args...)
			logfile := filepath.Join(dir, "calls")
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "RELAY_TEST_LOG="+logfile, "PAYESH_STATE_FILE="+filepath.Join(dir, "no-state"), "PAYESH_UPDATE_ENV="+filepath.Join(dir, "no-env"), "PAYESH_RELEASE_PUBLIC_KEY=", "PAYESH_RELEASE_KEY_ID=", "PAYESH_INSTALL_PROXY=", "PAYESH_RELAY_SSH=", "GITHUB_TOKEN=", "no_proxy=github.com")
			output, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatal("failed download was accepted")
			}
			data, err := os.ReadFile(logfile)
			if err != nil {
				t.Fatalf("download/relay not reached: %v: %s", err, output)
			}
			calls := string(data)
			if relay {
				for _, want := range []string{"-D 127.0.0.1:", "-p 2222 operator@relay.example.test", "proxy=socks5h://127.0.0.1:", "-O exit"} {
					if !strings.Contains(calls, want) {
						t.Fatalf("missing %q: %s", want, calls)
					}
				}
			} else if !strings.Contains(calls, "proxy=http://proxy.example.test:8080 bypass= ") {
				t.Fatalf("download bypassed proxy: %s", calls)
			}
		})
	}
}

func TestHubBootstrapRejectsAmbiguousRelayRoutes(t *testing.T) {
	for _, args := range [][]string{
		{"--proxy-url", "http://proxy.example.test:8080", "--relay-ssh", "relay.example.test"},
		{"--proxy-url", "file:///tmp/proxy"},
		{"--relay-ssh", "-oProxyCommand=bad"},
		{"--relay-ssh", "relay.example.test", "--relay-ssh-port", "0"},
	} {
		out, err := exec.Command("sh", append([]string{"../../install.sh"}, args...)...).CombinedOutput()
		if err == nil || strings.Contains(string(out), "UNSIGNED PREVIEW") {
			t.Fatalf("bad relay options not rejected before setup: %s %v", out, err)
		}
	}
}
