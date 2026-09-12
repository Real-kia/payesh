// Package install contains side-effect-free installer preflight checks. The
// executable installer can present these results before downloading anything.
package install

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

type Preflight struct {
	Role, Distribution, Version, Architecture, Init string
	Supported                                       bool
	Problems, Warnings, Artifacts                   []string
	Capabilities                                    Capabilities
}

// Capabilities describes host features that optional modules may require.
// These fields are diagnostic: lack of tc/nft/cgroup support must not prevent
// monitoring-only installation, but it must be visible to the caller.
type Capabilities struct {
	HostOS         string `json:"host_os"`
	Kernel         string `json:"kernel,omitempty"`
	PackageManager string `json:"package_manager,omitempty"`
	CgroupMode     string `json:"cgroup_mode,omitempty"`
	Privileged     bool   `json:"privileged"`
	HasIP          bool   `json:"has_ip"`
	HasTC          bool   `json:"has_tc"`
	HasNFT         bool   `json:"has_nft"`
}

func Check(root, role, listen string) (Preflight, error) {
	if root == "" {
		root = "/"
	}
	if role != "standalone" && role != "hub" && role != "node" && role != "cli-only" {
		return Preflight{}, errors.New("role must be standalone, hub, node, or cli-only")
	}
	p := Preflight{Role: role, Architecture: runtime.GOARCH, Capabilities: detectCapabilities(root)}
	hostSupported := true
	if root == "/" && p.Capabilities.HostOS != "linux" {
		hostSupported = false
		p.Problems = append(p.Problems, "host operating system is not Linux")
	}
	values, err := parseOSRelease(filepath.Join(root, "etc/os-release"))
	if err != nil {
		p.Problems = append(p.Problems, "could not read operating-system identity")
	} else {
		p.Distribution = values["ID"]
		p.Version = values["VERSION_ID"]
	}
	p.Init = detectInit(root)
	p.Supported = hostSupported && supported(p.Distribution, p.Version)
	if !p.Supported {
		p.Problems = append(p.Problems, "distribution is not in the launch support matrix")
	}
	if role != "cli-only" && p.Init == "unknown" {
		p.Supported = false
		p.Problems = append(p.Problems, "no supported init system was detected (systemd or openrc is required)")
	}
	if !supportedArchitecture(p.Architecture) {
		p.Supported = false
		p.Problems = append(p.Problems, "architecture is not supported")
	}
	p.Artifacts = []string{"payesh-agent", "payesh-privd", "payesh"}
	if role == "standalone" || role == "hub" {
		p.Artifacts = append(p.Artifacts, "payesh-server", "web-assets")
	}
	if role == "node" {
		p.Artifacts = []string{"payesh-agent", "payesh-privd", "payesh"}
	}
	if role == "cli-only" {
		p.Artifacts = []string{"payesh"}
	}
	if role != "node" && role != "cli-only" {
		if listen == "" {
			listen = "127.0.0.1:8787"
		}
		if err := validateListenAddress(listen); err != nil {
			p.Supported = false
			p.Problems = append(p.Problems, "requested listen address is invalid: "+err.Error())
		} else if occupied(listen) {
			p.Supported = false
			p.Problems = append(p.Problems, "requested listen address is already occupied")
		}
	}
	if !p.Capabilities.Privileged && role != "cli-only" {
		p.Warnings = append(p.Warnings, "installation needs root or an equivalent privileged service setup")
	}
	if p.Capabilities.PackageManager == "" && role != "cli-only" {
		p.Warnings = append(p.Warnings, "no supported package manager was detected; dependency installation may be unavailable")
	}
	if !p.Capabilities.HasIP {
		p.Warnings = append(p.Warnings, "ip tooling is unavailable; interface discovery and network controls will be unsupported")
	}
	if !p.Capabilities.HasTC {
		p.Warnings = append(p.Warnings, "tc tooling is unavailable; bandwidth controls will be unsupported")
	}
	if !p.Capabilities.HasNFT {
		p.Warnings = append(p.Warnings, "nft tooling is unavailable; port-traffic controls will be unsupported")
	}
	return p, nil
}

func validateListenAddress(address string) error {
	if strings.TrimSpace(address) != address || strings.ContainsAny(address, "\r\n\t\\\"'") {
		return errors.New("address contains unsafe whitespace or quoting")
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("must be host:port: %w", err)
	}
	if host == "" {
		return errors.New("host must not be empty")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return errors.New("port must be numeric and within 0..65535")
	}
	return nil
}

func supportedArchitecture(architecture string) bool {
	return architecture == "amd64" || architecture == "arm64"
}

func parseOSRelease(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	values := map[string]string{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		// os-release permits either single- or double-quoted values.  A few
		// minimal/container images use single quotes for VERSION_ID; leaving
		// those quotes attached makes an otherwise supported host fail the
		// support-matrix check.  Remove quotes only when they form a pair so
		// malformed values remain visible to the caller.
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			first, last := value[0], value[len(value)-1]
			if (first == '\'' && last == '\'') || (first == '"' && last == '"') {
				value = value[1 : len(value)-1]
			}
		}
		values[key] = value
	}
	return values, s.Err()
}
func detectInit(root string) string {
	if _, err := os.Stat(filepath.Join(root, "run/systemd/system")); err == nil {
		return "systemd"
	}
	if _, err := os.Stat(filepath.Join(root, "sbin/openrc")); err == nil {
		return "openrc"
	}
	return "unknown"
}

func detectCapabilities(root string) Capabilities {
	c := Capabilities{HostOS: runtime.GOOS, Privileged: os.Geteuid() == 0}
	if kernel, err := os.ReadFile(filepath.Join(root, "proc/sys/kernel/osrelease")); err == nil {
		c.Kernel = strings.TrimSpace(string(kernel))
	}
	for _, candidate := range []struct {
		name  string
		paths []string
	}{
		{"apt", []string{"usr/bin/apt-get", "usr/bin/apt"}},
		{"dnf", []string{"usr/bin/dnf", "usr/bin/yum"}},
		{"apk", []string{"sbin/apk", "usr/sbin/apk"}},
		{"rpm", []string{"usr/bin/rpm"}},
	} {
		for _, path := range candidate.paths {
			if executableAt(root, path) {
				c.PackageManager = candidate.name
				break
			}
		}
		if c.PackageManager != "" {
			break
		}
	}
	if _, err := os.Stat(filepath.Join(root, "sys/fs/cgroup/cgroup.controllers")); err == nil {
		c.CgroupMode = "v2"
	} else if _, err := os.Stat(filepath.Join(root, "sys/fs/cgroup")); err == nil {
		c.CgroupMode = "v1-or-unknown"
	}
	c.HasIP = executableAtAny(root, "sbin/ip", "usr/sbin/ip", "usr/bin/ip", "bin/ip")
	c.HasTC = executableAtAny(root, "sbin/tc", "usr/sbin/tc", "usr/bin/tc", "bin/tc")
	c.HasNFT = executableAtAny(root, "sbin/nft", "usr/sbin/nft", "usr/bin/nft", "bin/nft")
	return c
}

func executableAt(root, relative string) bool {
	info, err := os.Stat(filepath.Join(root, relative))
	return err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0
}

func executableAtAny(root string, paths ...string) bool {
	for _, path := range paths {
		if executableAt(root, path) {
			return true
		}
	}
	return false
}
func supported(id, version string) bool {
	major, minor, ok := releaseVersion(version)
	if !ok {
		return false
	}
	switch id {
	case "ubuntu":
		return (major == 22 && minor == 4) || (major == 24 && minor == 4)
	case "debian":
		return major == 12 || major == 13
	case "fedora":
		return major == 42 || major == 43
	case "rocky", "almalinux":
		return major == 9 || major == 10
	case "alpine":
		return major == 3 && (minor == 22 || minor == 23)
	}
	return false
}

// releaseVersion parses the numeric release identity from os-release. Patch
// components are intentionally ignored: the support matrix pins release
// families/streams (for example Ubuntu 22.04 and Alpine 3.22), not a single
// vendor point update. Unknown future families remain unsupported until they
// are explicitly added to the matrix and CI.
func releaseVersion(version string) (major, minor int, ok bool) {
	parts := strings.Split(strings.TrimSpace(version), ".")
	if len(parts) == 0 || len(parts) > 3 || parts[0] == "" {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil || major < 0 {
		return 0, 0, false
	}
	if len(parts) > 1 {
		minor, err = strconv.Atoi(parts[1])
		if err != nil || minor < 0 {
			return 0, 0, false
		}
	}
	if len(parts) > 2 {
		for _, part := range parts[2:] {
			if part == "" {
				return 0, 0, false
			}
			if _, err := strconv.Atoi(part); err != nil {
				return 0, 0, false
			}
		}
	}
	return major, minor, true
}

func atLeast(version string, major, minor int) bool {
	var gotMajor, gotMinor int
	if _, err := fmt.Sscanf(version, "%d.%d", &gotMajor, &gotMinor); err != nil {
		if _, err := fmt.Sscanf(version, "%d", &gotMajor); err != nil {
			return false
		}
	}
	return gotMajor > major || (gotMajor == major && gotMinor >= minor)
}
func occupied(address string) bool {
	// Port zero asks the kernel to choose an ephemeral port and therefore
	// cannot conflict with an existing listener. It is also useful for
	// side-effect-free fixture validation in restricted test environments.
	if _, port, err := net.SplitHostPort(address); err == nil && port == "0" {
		return false
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return true
	}
	_ = listener.Close()
	return false
}
func Format(p Preflight) string {
	return fmt.Sprintf("%s %s %s (%s, %s)", p.Role, p.Distribution, p.Version, p.Architecture, p.Init)
}
