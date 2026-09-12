// Command payesh-install performs installation preflight and, when explicitly
// requested, applies already downloaded/verified artifacts. It never downloads
// or trusts an artifact on the caller's behalf.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Real-kia/payesh/internal/install"
)

func main() {
	var artifactDigests digestFlags
	root := flag.String("root", "/", "target filesystem root")
	role := flag.String("role", "node", "installation role: standalone, hub, node, or cli-only")
	listen := flag.String("listen", "", "requested TCP listen address for web roles")
	jsonOutput := flag.Bool("json", false, "emit a machine-readable result")
	apply := flag.Bool("install", false, "install verified artifacts from --artifact-dir")
	artifactDir := flag.String("artifact-dir", "", "directory containing role-required verified artifacts")
	flag.Var(&artifactDigests, "artifact-sha256", "expected artifact digest as name=64-hex-sha256; repeat for every required artifact")
	start := flag.Bool("start", false, "enable and start installed services (requires --install)")
	uninstall := flag.Bool("uninstall", false, "stop/remove Payesh services and owned artifacts")
	removeData := flag.Bool("remove-data", false, "with --uninstall, also remove Payesh data/config/log directories")
	flag.Parse()
	if *uninstall && *apply {
		fmt.Fprintln(os.Stderr, "--uninstall cannot be combined with --install")
		os.Exit(2)
	}
	if *removeData && !*uninstall {
		fmt.Fprintln(os.Stderr, "--remove-data requires --uninstall")
		os.Exit(2)
	}
	if *start && !*apply {
		fmt.Fprintln(os.Stderr, "--start requires --install")
		os.Exit(2)
	}
	if *uninstall {
		result, err := install.Uninstall(context.Background(), install.UninstallOptions{
			Root: *root, Role: *role, RemoveData: *removeData, Stop: true,
		})
		if err != nil {
			if *jsonOutput {
				_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"error": err.Error()})
			} else {
				fmt.Fprintln(os.Stderr, "uninstall:", err)
			}
			if errors.Is(err, install.ErrUnsupported) {
				os.Exit(1)
			}
			os.Exit(2)
		}
		if *jsonOutput {
			if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
				fmt.Fprintln(os.Stderr, "write result:", err)
				os.Exit(2)
			}
		} else {
			fmt.Printf("removed: %v (data_preserved=%t)\n", result.Removed, result.DataPreserved)
		}
		return
	}

	if *apply {
		digests, digestErr := artifactDigests.parse()
		if digestErr != nil {
			fmt.Fprintln(os.Stderr, "install:", digestErr)
			os.Exit(2)
		}
		result, err := install.Install(context.Background(), install.InstallOptions{
			Root: *root, Role: *role, Listen: *listen, ArtifactDir: *artifactDir, Start: *start,
			Verify: func(name, path string) error {
				expected, ok := digests[name]
				if !ok {
					return fmt.Errorf("missing SHA-256 pin for %s", name)
				}
				actual, err := install.ArtifactDigest(path, name == "web-assets")
				if err != nil {
					return err
				}
				if actual != expected {
					return fmt.Errorf("SHA-256 mismatch for %s", name)
				}
				return nil
			},
		})
		if err != nil {
			if *jsonOutput {
				_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"error": err.Error()})
			} else {
				fmt.Fprintln(os.Stderr, "install:", err)
			}
			if errors.Is(err, install.ErrUnsupported) {
				os.Exit(1)
			}
			os.Exit(2)
		}
		if *jsonOutput {
			if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
				fmt.Fprintln(os.Stderr, "write result:", err)
				os.Exit(2)
			}
		} else {
			fmt.Printf("installed: %v\n", result.Installed)
			fmt.Printf("services: %v (started=%t)\n", result.Services, result.Started)
		}
		return
	}

	preflight, err := install.Check(*root, *role, *listen)
	if err != nil {
		if *jsonOutput {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"error": err.Error()})
		} else {
			fmt.Fprintln(os.Stderr, "preflight:", err)
		}
		os.Exit(2)
	}
	if *jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(preflight); err != nil {
			fmt.Fprintln(os.Stderr, "write result:", err)
			os.Exit(2)
		}
	} else {
		fmt.Printf("platform: %s\n", install.Format(preflight))
		fmt.Printf("supported: %t\n", preflight.Supported)
		fmt.Printf("capabilities: os=%s kernel=%s package_manager=%s cgroup=%s privileged=%t ip=%t tc=%t nft=%t\n", preflight.Capabilities.HostOS, preflight.Capabilities.Kernel, preflight.Capabilities.PackageManager, preflight.Capabilities.CgroupMode, preflight.Capabilities.Privileged, preflight.Capabilities.HasIP, preflight.Capabilities.HasTC, preflight.Capabilities.HasNFT)
		fmt.Printf("artifacts: %v\n", preflight.Artifacts)
		for _, problem := range preflight.Problems {
			fmt.Printf("problem: %s\n", problem)
		}
		for _, warning := range preflight.Warnings {
			fmt.Printf("warning: %s\n", warning)
		}
	}
	if !preflight.Supported {
		os.Exit(1)
	}
}

type digestFlags []string

func (d *digestFlags) String() string { return strings.Join(*d, ",") }
func (d *digestFlags) Set(value string) error {
	*d = append(*d, value)
	return nil
}

func (d digestFlags) parse() (map[string]string, error) {
	parsed := make(map[string]string, len(d))
	for _, value := range d {
		name, digest, ok := strings.Cut(value, "=")
		if !ok || name == "" || len(digest) != 64 {
			return nil, errors.New("--artifact-sha256 must use name=64-hex-sha256")
		}
		for _, character := range digest {
			if !strings.ContainsRune("0123456789abcdef", character) {
				return nil, errors.New("--artifact-sha256 digest must be lowercase hexadecimal")
			}
		}
		if _, exists := parsed[name]; exists {
			return nil, fmt.Errorf("duplicate SHA-256 pin for %s", name)
		}
		parsed[name] = digest
	}
	return parsed, nil
}
