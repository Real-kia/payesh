package install

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckSelectsRoleArtifactsAndDetectsPlatform(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/os-release"), []byte("ID=debian\nVERSION_ID=12\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "run/systemd/system"), 0700); err != nil {
		t.Fatal(err)
	}
	p, err := Check(root, "node", "")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Supported || p.Init != "systemd" {
		t.Fatalf("preflight=%+v", p)
	}
	for _, artifact := range p.Artifacts {
		if artifact == "payesh-server" || artifact == "web-assets" {
			t.Fatal("node includes web artifact")
		}
	}
	if _, err := Check(root, "invalid", ""); err == nil {
		t.Fatal("invalid role accepted")
	}
}

func TestCheckRejectsUnsafeOrMalformedListenAddress(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc", "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "run/systemd/system"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/os-release"), []byte("ID=debian\nVERSION_ID=12\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, listen := range []string{"127.0.0.1", "127.0.0.1:65536", "127.0.0.1:80\nExecStart=/bin/sh", "127.0.0.1:bad"} {
		p, err := Check(root, "standalone", listen)
		if err != nil || p.Supported {
			t.Fatalf("unsafe listener accepted: %q preflight=%+v err=%v", listen, p, err)
		}
	}
}

func TestCheckRejectsSupportedDistributionWithoutInitSystem(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/os-release"), []byte("ID=debian\nVERSION_ID=12\n"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := Check(root, "node", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Supported {
		t.Fatalf("preflight unexpectedly supported without init: %+v", p)
	}
	if len(p.Problems) == 0 || p.Problems[len(p.Problems)-1] != "no supported init system was detected (systemd or openrc is required)" {
		t.Fatalf("missing init problem: %+v", p.Problems)
	}
}

func TestSupportedReleaseMatrixIsPinned(t *testing.T) {
	for _, tc := range []struct {
		id, version string
		want        bool
	}{
		{"ubuntu", "22.04", true},
		{"ubuntu", "24.04", true},
		{"ubuntu", "23.10", false},
		{"ubuntu", "25.04", false},
		{"debian", "12", true},
		{"debian", "13", true},
		{"debian", "14", false},
		{"fedora", "42", true},
		{"fedora", "43", true},
		{"fedora", "44", false},
		{"fedora", "39", false},
		{"rocky", "9", true},
		{"rocky", "10.2", true},
		{"rocky", "11", false},
		{"almalinux", "9", true},
		{"almalinux", "10", true},
		{"alpine", "3.22", true},
		{"alpine", "3.23.1", true},
		{"alpine", "3.24", false},
	} {
		if got := supported(tc.id, tc.version); got != tc.want {
			t.Errorf("supported(%q, %q)=%v, want %v", tc.id, tc.version, got, tc.want)
		}
	}
}

func TestSupportedArchitectureMatrix(t *testing.T) {
	if !supportedArchitecture("amd64") || !supportedArchitecture("arm64") {
		t.Fatal("declared release architectures were rejected")
	}
	for _, architecture := range []string{"386", "arm", "ppc64le", "riscv64", "wasm"} {
		if supportedArchitecture(architecture) {
			t.Fatalf("unsupported architecture accepted: %s", architecture)
		}
	}
}

func TestCheckReportsOptionalCapabilitiesWithoutBlockingSupportedHost(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"etc", "run/systemd/system", "usr/bin", "proc/sys/kernel", "sys/fs/cgroup"} {
		if err := os.MkdirAll(filepath.Join(root, path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "etc/os-release"), []byte("ID=debian\nVERSION_ID=12\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc/sys/kernel/osrelease"), []byte("6.1-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "usr/bin/apt-get"), []byte("fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "usr/bin/ip"), []byte("fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := Check(root, "node", "")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Supported || p.Capabilities.PackageManager != "apt" || p.Capabilities.Kernel != "6.1-test" || !p.Capabilities.HasIP || p.Capabilities.HasTC || p.Capabilities.HasNFT {
		t.Fatalf("unexpected capability report: %+v", p)
	}
	if len(p.Warnings) == 0 {
		t.Fatal("missing unsupported capability warnings")
	}
}
