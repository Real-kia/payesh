package install

import (
	"context"
	"strings"
	"testing"
)

type controlTransport struct {
	key      SSHHostKey
	commands []string
}

func (c *controlTransport) Scan(context.Context, SSHEndpoint) ([]SSHHostKey, error) {
	return []SSHHostKey{c.key}, nil
}
func (c *controlTransport) Upload(context.Context, SSHEndpoint, string, SSHAuth, string, string, bool) error {
	return nil
}
func (c *controlTransport) Run(_ context.Context, _ SSHEndpoint, _ string, _ SSHAuth, command string, _ []byte) ([]byte, error) {
	c.commands = append(c.commands, command)
	if command == "id -u" {
		return []byte("0\n"), nil
	}
	return nil, nil
}

func TestControlNodeOverSSHUsesFixedCommandAndVerifiedHost(t *testing.T) {
	key := testHostKey("node.example", 22, 1)
	transport := &controlTransport{key: key}
	endpoint := SSHEndpoint{Host: "node.example", Port: 22, User: "root"}
	for _, action := range []string{"restart", "disable", "enable"} {
		if err := ControlNodeOverSSH(context.Background(), endpoint, SSHAuth{Password: []byte("password")}, key.Fingerprint, "", action, transport); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(transport.commands, ";"); !strings.Contains(got, "systemctl restart payesh-agent") || !strings.Contains(got, "systemctl disable --now payesh-agent") || !strings.Contains(got, "systemctl enable --now payesh-agent") {
		t.Fatal(got)
	}
	if err := ControlNodeOverSSH(context.Background(), endpoint, SSHAuth{Password: []byte("password")}, "SHA256:wrong", "", "restart", transport); err == nil {
		t.Fatal("accepted wrong host fingerprint")
	}
	if err := ControlNodeOverSSH(context.Background(), endpoint, SSHAuth{Password: []byte("password")}, key.Fingerprint, "", "arbitrary", transport); err == nil {
		t.Fatal("accepted arbitrary command")
	}
}
