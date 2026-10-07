package install

import (
	"os/exec"
	"strings"
	"testing"
)

// The join options must be rejected before anything is downloaded or changed.
func TestInstallScriptRejectsMalformedJoinOptions(t *testing.T) {
	valid := []string{"--role", "node", "--join-url", "wss://hub.example:9797/node/v1", "--join-job", "enrollment-0123abcd", "--join-token", "AbCdEfGhIjKlMnOpQrSt"}
	override := func(flag, value string) []string {
		out := append([]string{}, valid...)
		for i := 0; i+1 < len(out); i++ {
			if out[i] == flag {
				out[i+1] = value
			}
		}
		return out
	}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"missing token", []string{"--role", "node", "--join-url", "wss://hub.example:9797/node/v1", "--join-job", "enrollment-1"}, "needs --join-url, --join-job and --join-token together"},
		{"not a node", override("--role", "standalone"), "require --role node"},
		{"plain ws", override("--join-url", "ws://hub.example:9797/node/v1"), "must look like wss://"},
		{"wrong path", override("--join-url", "wss://hub.example:9797/other"), "must look like wss://"},
		{"host injection", override("--join-url", "wss://hub.example;rm/node/v1"), "unexpected host"},
		{"job injection", override("--join-job", "job\nPAYESH_X=1"), "unexpected characters"},
		{"token injection", override("--join-token", "AbCdEfGhIjKlMnOp$(id)"), "unexpected characters"},
		{"short token", override("--join-token", "short"), "unexpected length"},
		{"bad pin", append(append([]string{}, valid...), "--join-ca-sha256", "xyz"), "64 hex characters"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := exec.Command("sh", append([]string{"../../install.sh", "--release-mode", "preview"}, tc.args...)...).CombinedOutput()
			if err == nil {
				t.Fatalf("install.sh accepted %v: %s", tc.args, out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Fatalf("output %q does not contain %q", out, tc.want)
			}
		})
	}
}
