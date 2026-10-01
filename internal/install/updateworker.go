package install

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/Real-kia/payesh/internal/webupdate"
)

const updateWorkerService = webupdate.ServiceName

// updateWorkerRoles reports whether role hosts the web UI and therefore the
// root worker that applies updates requested from it.
func updateWorkerRole(role string) bool { return role == "standalone" || role == "hub" }

func updateWorkerDefinition(init string) (string, bool) {
	switch init {
	case "systemd":
		return "[Unit]\nDescription=Payesh web update worker\nAfter=network-online.target\nWants=network-online.target\n\n[Service]\nType=simple\nUser=root\nGroup=root\nEnvironmentFile=-/etc/payesh/payesh.env\nExecStart=/usr/bin/payesh update-worker\nRestart=always\nRestartSec=5s\n\n[Install]\nWantedBy=multi-user.target\n", true
	case "openrc":
		return "#!/sbin/openrc-run\nname=\"payesh-update\"\ndescription=\"Payesh web update worker\"\ncommand=\"/bin/sh\"\ncommand_args=\"-c 'if [ -f /etc/payesh/payesh.env ]; then set -a; . /etc/payesh/payesh.env; set +a; fi; exec /usr/bin/payesh update-worker'\"\nsupervisor=\"supervise-daemon\"\nsupervise_daemon_args=\"--respawn-delay 5\"\noutput_log=\"/var/log/payesh/update.log\"\nerror_log=\"/var/log/payesh/update.err\"\n\ndepend() {\n\tneed net\n}\n", true
	}
	return "", false
}

// installUpdateWorker writes the worker definition and, on the live root,
// enables it and starts it if it is not already running. It is never
// restarted here: the worker may be the process running this installer, and it
// picks up the new binary by exiting after a successful update.
func installUpdateWorker(ctx context.Context, root, init string, start bool, runner CommandRunner) error {
	content, ok := updateWorkerDefinition(init)
	if !ok {
		return &UnsupportedError{Reason: "no update worker definition for " + init}
	}
	if err := writeAtomic(servicePath(root, init, updateWorkerService), []byte(content), serviceMode(init)); err != nil {
		return err
	}
	if !start || root != "/" {
		return nil
	}
	if init == "systemd" {
		for _, args := range [][]string{{"daemon-reload"}, {"enable", updateWorkerService}, {"start", updateWorkerService}} {
			if out, err := runner.Run(ctx, "systemctl", args...); err != nil {
				return commandError("systemctl "+strings.Join(args, " "), out, err)
			}
		}
		return nil
	}
	if out, err := runner.Run(ctx, "rc-update", "add", updateWorkerService, "default"); err != nil {
		return commandError("rc-update add "+updateWorkerService, out, err)
	}
	if out, err := runner.Run(ctx, "rc-service", updateWorkerService, "start"); err != nil {
		return commandError("rc-service start "+updateWorkerService, out, err)
	}
	return nil
}

func removeUpdateWorker(ctx context.Context, root, init string, runner CommandRunner) error {
	path := servicePath(root, init, updateWorkerService)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if root == "/" {
		if init == "systemd" {
			_, _ = runner.Run(ctx, "systemctl", "disable", "--now", updateWorkerService)
		} else if init == "openrc" {
			_, _ = runner.Run(ctx, "rc-service", updateWorkerService, "stop")
			_, _ = runner.Run(ctx, "rc-update", "del", updateWorkerService, "default")
		}
	}
	_, err := removeOwnedPath(path, root)
	return err
}
