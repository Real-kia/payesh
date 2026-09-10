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
	Problems, Artifacts                             []string
}

func Check(root, role, listen string) (Preflight, error) {
	if root == "" {
		root = "/"
	}
	if role != "standalone" && role != "hub" && role != "node" && role != "cli-only" {
		return Preflight{}, errors.New("role must be standalone, hub, node, or cli-only")
	}
	p := Preflight{Role: role, Architecture: runtime.GOARCH}
	values, err := parseOSRelease(filepath.Join(root, "etc/os-release"))
	if err != nil {
		p.Problems = append(p.Problems, "could not read operating-system identity")
	} else {
		p.Distribution = values["ID"]
		p.Version = values["VERSION_ID"]
	}
	p.Init = detectInit(root)
	p.Supported = supported(p.Distribution, p.Version)
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
	if role != "node" && role != "cli-only" {
		if listen == "" {
			listen = "127.0.0.1:8787"
		}
		if occupied(listen) {
			p.Supported = false
			p.Problems = append(p.Problems, "requested listen address is already occupied")
		}
	}
	return p, nil
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
		values[key] = strings.Trim(value, "\"")
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
