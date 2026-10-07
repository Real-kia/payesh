package webupdate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const InstallationScopePath = "/etc/payesh-installation.json"

type InstallationScope struct {
	Role string `json:"role"`
	Init string `json:"init"`
}

func (s InstallationScope) valid() bool {
	// CLI-only installations own no services and can run on supported hosts
	// without an init manager. Preserve that actual host state in the scope.
	if s.Role == "cli-only" && s.Init == "unknown" {
		return true
	}
	return (s.Role == "node" || s.Role == "hub" || s.Role == "standalone" || s.Role == "cli-only") && (s.Init == "systemd" || s.Init == "openrc")
}
func scopePath(root string) string {
	return filepath.Join(root, strings.TrimPrefix(InstallationScopePath, "/"))
}
func WriteInstallationScope(root string, scope InstallationScope) error {
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root || !scope.valid() {
		return errors.New("invalid installation scope")
	}
	path := scopePath(root)
	if err := safeExistingParents(filepath.Dir(path), root == "/"); err != nil {
		return err
	}
	return writeJSON(filepath.Dir(path), filepath.Base(path), scope, 0644)
}
func ReadInstallationScope(root string) (InstallationScope, error) {
	var scope InstallationScope
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return scope, errors.New("invalid installation root")
	}
	path := scopePath(root)
	if err := safeExistingParents(path, root == "/"); err != nil {
		return scope, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return scope, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return scope, errors.New("installation scope is not protected")
	}
	if err = readJSON(path, &scope); err != nil {
		return scope, err
	}
	if !scope.valid() {
		return scope, errors.New("invalid protected installation scope")
	}
	return scope, nil
}
