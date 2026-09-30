package install

import (
	"context"
	"errors"
	"os"
	"strings"
)

// ControlNodeOverSSH runs one fixed service operation using one-time SSH
// credentials. It never accepts a caller-supplied shell command.
func ControlNodeOverSSH(ctx context.Context, endpoint SSHEndpoint, auth SSHAuth, fingerprint, knownHostsPath, action string, transport SSHTransport) error {
	defer clearSSHAuth(&auth)
	if err := validateSSHTarget(endpoint); err != nil {
		return err
	}
	if err := validateSSHAuth(auth); err != nil {
		return err
	}
	command := ""
	switch action {
	case "restart":
		command = "systemctl restart payesh-agent"
	case "disable":
		command = "systemctl disable --now payesh-agent"
	case "enable":
		command = "systemctl enable --now payesh-agent"
	default:
		return errors.New("unsupported node service action")
	}
	if transport == nil {
		transport = OpenSSHTransport{}
	}
	keys, err := transport.Scan(ctx, endpoint)
	if err != nil {
		return sshStage("scan host key", err)
	}
	trusted, err := VerifyHostKeys(endpoint.Host, endpoint.Port, keys, knownHostsPath, fingerprint, nil)
	if err != nil {
		return sshStage("verify host key", err)
	}
	knownHosts, err := writeTransientKnownHosts(trusted)
	if err != nil {
		return err
	}
	defer os.Remove(knownHosts)
	uid, err := transport.Run(ctx, endpoint, knownHosts, auth, "id -u", nil)
	if err != nil {
		return sshStage("check remote privileges", err)
	}
	if strings.TrimSpace(string(uid)) != "0" {
		command = "sudo -n " + command
	}
	if _, err := transport.Run(ctx, endpoint, knownHosts, auth, command, nil); err != nil {
		return sshStage("control node service", err)
	}
	return nil
}
