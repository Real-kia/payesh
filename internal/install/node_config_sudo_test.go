package install

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// A multi-line script prefixed with "sudo" only elevates its first line, so for
// a non-root SSH user every later line would run unprivileged and fail without
// the shell reporting it. The whole script must be one elevated command.
func TestApplyNodeConfigElevatesTheWholeScriptAsOneCommand(t *testing.T) {
	transport := &fakeSSHTransport{}
	opts := SSHInstallOptions{Role: "node"}
	if err := applyNodeConfig(context.Background(), transport, SSHEndpoint{}, "", SSHAuth{}, "sudo -n ", nil, "/var/tmp/stage", opts); err != nil {
		t.Fatal(err)
	}
	if len(transport.commands) != 1 {
		t.Fatalf("commands=%q", transport.commands)
	}
	command := transport.commands[0]
	if !strings.HasPrefix(command, "sudo -n sh -c '") || !strings.HasSuffix(command, "'") {
		t.Fatalf("script is not a single elevated command: %q", command)
	}
	if !strings.Contains(command, "rc-service payesh-agent restart") {
		t.Fatalf("OpenRC hosts are never restarted: %q", command)
	}
	if err := exec.Command("sh", "-n", "-c", strings.TrimPrefix(command, "sudo -n ")).Run(); err != nil {
		t.Fatalf("generated command is not valid shell: %v", err)
	}
}
