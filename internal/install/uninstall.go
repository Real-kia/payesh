package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ServiceRemover is the privileged part of uninstall.  It is injectable so
// disposable roots can verify the exact service calls without touching the
// host supervisor.
type ServiceRemover interface {
	Remove(context.Context, string, string, []string, bool) error
}

// UninstallOptions controls removal of only Payesh-owned paths.  Data is
// retained by default; removing it is a separate, explicit opt-in.
type UninstallOptions struct {
	Root           string
	Role           string
	RemoveData     bool
	Stop           bool
	ServiceRemover ServiceRemover
	CommandRunner  CommandRunner
}

type UninstallResult struct {
	Role          string   `json:"role"`
	Init          string   `json:"init"`
	Removed       []string `json:"removed"`
	DataPreserved bool     `json:"data_preserved"`
	Stopped       bool     `json:"stopped"`
}

// Uninstall removes service definitions and exact Payesh-owned artifacts. It
// never follows symlinks, never uses a wildcard, and preserves the database,
// identity, configuration, and logs unless RemoveData is explicitly set.
func Uninstall(ctx context.Context, opts UninstallOptions) (UninstallResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	root := opts.Root
	if root == "" {
		root = "/"
	}
	role := strings.TrimSpace(opts.Role)
	if role != "standalone" && role != "hub" && role != "node" {
		return UninstallResult{}, errors.New("role must be standalone, hub, or node")
	}
	init := detectInit(root)
	if init == "unknown" {
		return UninstallResult{}, &UnsupportedError{Reason: "no supported init system was detected"}
	}
	services := serviceNames(role)
	result := UninstallResult{Role: role, Init: init, DataPreserved: !opts.RemoveData}
	if opts.Stop {
		remover := opts.ServiceRemover
		if remover == nil {
			remover = commandServiceRemover{runner: chooseRunner(opts.CommandRunner)}
		}
		if err := remover.Remove(ctx, root, init, services, true); err != nil {
			return result, fmt.Errorf("stop and disable services: %w", err)
		}
		result.Stopped = true
	}

	paths := make([]string, 0, len(services)+4)
	for _, service := range services {
		paths = append(paths, servicePath(root, init, service))
	}
	for _, name := range pArtifacts(role) {
		paths = append(paths, artifactDestination(root, name))
	}
	if opts.RemoveData {
		paths = append(paths, rooted(root, dataDir), rooted(root, configDir), rooted(root, logDir))
	}
	for _, path := range paths {
		removed, err := removeOwnedPath(path, root)
		if err != nil {
			return result, fmt.Errorf("remove %s: %w", path, err)
		}
		if removed {
			result.Removed = append(result.Removed, path)
		}
	}
	return result, nil
}

func pArtifacts(role string) []string {
	switch role {
	case "standalone", "hub":
		return []string{"payesh-agent", "payesh-privd", "payesh", "payesh-server", "web-assets"}
	case "node":
		return []string{"payesh-agent", "payesh-privd", "payesh"}
	default:
		return nil
	}
}

func removeOwnedPath(path, root string) (bool, error) {
	clean := filepath.Clean(path)
	if clean == string(filepath.Separator) || root == "" {
		return false, errors.New("refusing unsafe removal path")
	}
	info, err := os.Lstat(clean)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("refusing to remove symlink")
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return false, errors.New("refusing to remove special file")
	}
	// Every destination is produced by rooted() and is therefore anchored
	// beneath root for fixture roots. Keep this check as a second guard.
	if root != "/" {
		rel, relErr := filepath.Rel(filepath.Clean(root), clean)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return false, errors.New("removal path escapes target root")
		}
	}
	if err := os.RemoveAll(clean); err != nil {
		return false, err
	}
	return true, nil
}

type commandServiceRemover struct{ runner CommandRunner }

func (m commandServiceRemover) Remove(ctx context.Context, root, init string, services []string, stop bool) error {
	if root != "/" {
		return &UnsupportedError{Reason: "stopping services requires the live root (use an injected service remover for a fixture root)"}
	}
	if init == "systemd" {
		if stop {
			for _, service := range services {
				if out, err := m.runner.Run(ctx, "systemctl", "disable", "--now", service); err != nil {
					return commandError("systemctl disable --now "+service, out, err)
				}
			}
		}
		if out, err := m.runner.Run(ctx, "systemctl", "daemon-reload"); err != nil {
			return commandError("systemctl daemon-reload", out, err)
		}
		return nil
	}
	if init != "openrc" {
		return &UnsupportedError{Reason: "service manager " + init}
	}
	for _, service := range services {
		if stop {
			if out, err := m.runner.Run(ctx, "rc-service", service, "stop"); err != nil {
				return commandError("rc-service stop "+service, out, err)
			}
		}
		if out, err := m.runner.Run(ctx, "rc-update", "del", service, "default"); err != nil {
			return commandError("rc-update del "+service, out, err)
		}
	}
	return nil
}
