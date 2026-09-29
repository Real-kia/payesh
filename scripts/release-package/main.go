// Command release-package builds the Linux release artifacts and writes a
// deterministic, unsigned local release bundle. Signing is deliberately not
// attempted here: production Ed25519 keys must remain outside this checkout.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type artifact struct {
	Name            string `json:"name"`
	OS              string `json:"os"`
	Arch            string `json:"arch"`
	SHA256          string `json:"sha256"`
	CompressedBytes uint64 `json:"compressed_bytes,string"`
	UnpackedBytes   uint64 `json:"unpacked_bytes,string"`
	URL             string `json:"url"`
}

type manifest struct {
	Format       string     `json:"format"`
	Release      string     `json:"release"`
	CreatedAt    time.Time  `json:"created_at"`
	MinCore      string     `json:"min_core"`
	Artifacts    []artifact `json:"artifacts"`
	SigningKeyID string     `json:"signing_key_id"`
}

type builtArtifact struct {
	name  string
	path  string
	bytes uint64
}

var targets = []struct {
	name string
	path string
}{
	{"payesh-agent", "./cmd/payesh-agent"},
	{"payesh-server", "./cmd/payesh-server"},
	{"payesh", "./cmd/payesh"},
	{"payesh-privd", "./cmd/payesh-privd"},
	{"payesh-install", "./cmd/payesh-install"},
	{"payesh-updater-watchdog", "./cmd/payesh-updater-watchdog"},
	{"bandwidth-controls", "./cmd/payesh-bandwidth-module"},
	{"cpu-controls", "./cmd/payesh-cpu-module"},
	{"port-traffic", "./cmd/payesh-port-traffic"},
}

func main() {
	flags := flag.NewFlagSet("release-package", flag.ExitOnError)
	version := flags.String("version", envOr("PAYESH_RELEASE_VERSION", "0.1.0"), "semantic release version")
	out := flags.String("out", envOr("PAYESH_RELEASE_OUT", "dist/releases"), "release output directory")
	baseURL := flags.String("base-url", os.Getenv("PAYESH_RELEASE_BASE_URL"), "published artifact URL prefix (defaults to the repository release URL)")
	created := flags.String("created-at", os.Getenv("PAYESH_RELEASE_CREATED_AT"), "RFC3339 manifest timestamp (defaults to SOURCE_DATE_EPOCH or the HEAD commit time)")
	includeWeb := flags.Bool("include-web", os.Getenv("PAYESH_SKIP_WEB") != "1", "include web/dist as the web-assets artifact")
	flags.Parse(os.Args[1:])

	if err := run(*version, *out, *baseURL, *created, *includeWeb); err != nil {
		fmt.Fprintln(os.Stderr, "release-package:", err)
		os.Exit(1)
	}
}

func run(version, output, baseURL, created string, includeWeb bool) error {
	if !validVersion(version) {
		return errors.New("version must be MAJOR.MINOR.PATCH with an optional prerelease suffix")
	}
	when, err := releaseTime(created)
	if err != nil {
		return err
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	if baseURL == "" {
		baseURL = "https://github.com/Real-kia/payesh/releases/download/v" + version
	}
	baseURL = strings.TrimRight(baseURL, "/")
	outputDir, err := resolveOutputDir(output)
	if err != nil {
		return err
	}
	releaseDir := filepath.Join(outputDir, version)
	if info, statErr := os.Lstat(releaseDir); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to write release through symlink %q", releaseDir)
		}
		return fmt.Errorf("release directory already exists; refusing to overwrite %q", releaseDir)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect release destination: %w", statErr)
	}
	// Both temporary directories are created under the validated output root.
	// Only these exact paths are cleaned up. The final release is never removed
	// or replaced by this command.
	candidateDir, err := os.MkdirTemp(outputDir, ".release-candidate-")
	if err != nil {
		return fmt.Errorf("create release candidate: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(candidateDir)
		}
	}()
	workDir, err := os.MkdirTemp(outputDir, ".release-work-")
	if err != nil {
		return fmt.Errorf("create release work directory: %w", err)
	}
	defer os.RemoveAll(workDir)
	releaseDir = candidateDir

	var all []builtArtifact
	for _, arch := range []string{"amd64", "arm64"} {
		archDir := filepath.Join(workDir, arch)
		if err := os.MkdirAll(archDir, 0o755); err != nil {
			return err
		}
		for _, target := range targets {
			binary := filepath.Join(archDir, target.name)
			if err := build(root, target.path, binary, arch, version); err != nil {
				return fmt.Errorf("build %s linux/%s: %w", target.name, arch, err)
			}
			archiveName := target.name + "-linux-" + arch + ".tar.gz"
			archivePath := filepath.Join(releaseDir, archiveName)
			built, err := archiveBinary(binary, target.name, archivePath, when)
			if err != nil {
				return fmt.Errorf("archive %s: %w", target.name, err)
			}
			all = append(all, builtArtifact{name: target.name, path: archiveName, bytes: built})
		}
		if includeWeb {
			webDir := filepath.Join(root, "web", "dist")
			if _, statErr := os.Stat(webDir); statErr != nil {
				return fmt.Errorf("web/dist is unavailable; run `npm ci --ignore-scripts --no-audit --no-fund` and `make web-check`, or set PAYESH_SKIP_WEB=1: %w", statErr)
			}
			archiveName := "web-assets-linux-" + arch + ".tar.gz"
			archivePath := filepath.Join(releaseDir, archiveName)
			unpacked, err := archiveTree(webDir, "web-assets", archivePath, when)
			if err != nil {
				return fmt.Errorf("archive web assets: %w", err)
			}
			all = append(all, builtArtifact{name: "web-assets", path: archiveName, bytes: unpacked})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].name != all[j].name {
			return all[i].name < all[j].name
		}
		return all[i].path < all[j].path
	})

	m := manifest{Format: "payesh.release.v1", Release: version, CreatedAt: when, MinCore: version, SigningKeyID: "unavailable-local"}
	for _, built := range all {
		path := filepath.Join(releaseDir, built.path)
		digest, size, err := digestFile(path)
		if err != nil {
			return err
		}
		m.Artifacts = append(m.Artifacts, artifact{Name: built.name, OS: "linux", Arch: archFromPath(built.path), SHA256: digest, CompressedBytes: size, UnpackedBytes: built.bytes, URL: baseURL + "/" + built.path})
	}
	manifestBytes, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	manifestBytes = append(manifestBytes, '\n')
	if err := os.WriteFile(filepath.Join(releaseDir, "manifest.json"), manifestBytes, 0o644); err != nil {
		return err
	}
	if err := writeChecksums(releaseDir, m.Artifacts); err != nil {
		return err
	}
	if err := os.Chmod(candidateDir, 0o755); err != nil {
		return fmt.Errorf("set release candidate permissions: %w", err)
	}
	finalDir := filepath.Join(outputDir, version)
	if info, statErr := os.Lstat(finalDir); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to publish through symlink %q", finalDir)
		}
		return fmt.Errorf("release directory appeared during build; refusing to overwrite %q", finalDir)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect release destination before publish: %w", statErr)
	}
	if err := os.Rename(candidateDir, finalDir); err != nil {
		return fmt.Errorf("publish release atomically: %w", err)
	}
	published = true
	fmt.Printf("release=%s artifacts=%d output=%s\n", version, len(m.Artifacts), finalDir)
	fmt.Println("signing=not-created (production Ed25519 key is intentionally external)")
	return nil
}

func resolveOutputDir(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("output directory is required")
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("resolve output directory: %w", err)
	}
	output := filepath.Clean(abs)
	if output == string(filepath.Separator) {
		return "", errors.New("refusing filesystem root as release output")
	}
	// Resolve the nearest existing ancestor before creating anything. This
	// rejects symlinked parents rather than accidentally writing elsewhere.
	ancestor := output
	for {
		info, statErr := os.Lstat(ancestor)
		if statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 && ancestor == output {
				return "", fmt.Errorf("output directory or parent is a symlink: %q", ancestor)
			}
			if !info.IsDir() {
				return "", fmt.Errorf("output parent is not a directory: %q", ancestor)
			}
			_, evalErr := filepath.EvalSymlinks(ancestor)
			if evalErr != nil {
				return "", fmt.Errorf("resolve output parent: %w", evalErr)
			}
			break
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return "", fmt.Errorf("inspect output parent: %w", statErr)
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", errors.New("could not resolve output parent")
		}
		ancestor = parent
	}
	if err := os.MkdirAll(output, 0o755); err != nil {
		return "", fmt.Errorf("create output directory: %w", err)
	}
	info, err := os.Lstat(output)
	if err != nil {
		return "", fmt.Errorf("inspect output directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("output is not a regular directory: %q", output)
	}
	resolved, err := filepath.EvalSymlinks(output)
	if err != nil {
		return "", fmt.Errorf("output directory resolves through a symlink: %q", output)
	}
	resolved = filepath.Clean(resolved)
	if resolved == string(filepath.Separator) {
		return "", errors.New("refusing filesystem root as resolved release output")
	}
	return resolved, nil
}

func build(root, packagePath, output, arch, version string) error {
	ldflags := "-buildid= -X github.com/Real-kia/payesh/internal/version.Value=" + version
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-ldflags="+ldflags, "-o", output, packagePath)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func archiveBinary(source, name, destination string, when time.Time) (uint64, error) {
	info, err := os.Stat(source)
	if err != nil {
		return 0, err
	}
	file, err := os.Open(source)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	out, err := os.Create(destination)
	if err != nil {
		return 0, err
	}
	if err := writeTarGz(out, func(tw *tar.Writer) error {
		header := &tar.Header{Name: name, Mode: 0o755, Size: info.Size(), ModTime: when, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		_, err := io.Copy(tw, file)
		return err
	}); err != nil {
		out.Close()
		return 0, err
	}
	if err := out.Close(); err != nil {
		return 0, err
	}
	return uint64(info.Size()), nil
}

func archiveTree(source, prefix, destination string, when time.Time) (uint64, error) {
	var paths []string
	if err := filepath.Walk(source, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path != source && !info.IsDir() {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("web asset %q is not a regular file", path)
			}
			paths = append(paths, path)
		}
		return nil
	}); err != nil {
		return 0, err
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return 0, errors.New("web/dist contains no regular files")
	}
	out, err := os.Create(destination)
	if err != nil {
		return 0, err
	}
	var unpacked uint64
	err = writeTarGz(out, func(tw *tar.Writer) error {
		for _, path := range paths {
			info, statErr := os.Stat(path)
			if statErr != nil {
				return statErr
			}
			rel, relErr := filepath.Rel(source, path)
			if relErr != nil {
				return relErr
			}
			name := filepath.ToSlash(filepath.Join(prefix, rel))
			header := &tar.Header{Name: name, Mode: 0o644, Size: info.Size(), ModTime: when, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}
			if err := tw.WriteHeader(header); err != nil {
				return err
			}
			file, openErr := os.Open(path)
			if openErr != nil {
				return openErr
			}
			_, copyErr := io.Copy(tw, file)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			if info.Size() < 0 || uint64(info.Size()) > ^uint64(0)-unpacked {
				return errors.New("web asset size overflow")
			}
			unpacked += uint64(info.Size())
		}
		return nil
	})
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	return unpacked, err
}

func writeTarGz(out *os.File, write func(*tar.Writer) error) error {
	gz := gzip.NewWriter(out)
	gz.Header.ModTime = time.Unix(0, 0).UTC()
	gz.Header.Name, gz.Header.Comment, gz.Header.Extra = "", "", nil
	tarWriter := tar.NewWriter(gz)
	err := write(tarWriter)
	if closeErr := tarWriter.Close(); err == nil {
		err = closeErr
	}
	if closeErr := gz.Close(); err == nil {
		err = closeErr
	}
	return err
}

func digestFile(path string) (string, uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), uint64(n), nil
}

func writeChecksums(dir string, artifacts []artifact) error {
	lines := make([]string, 0, len(artifacts))
	for _, a := range artifacts {
		lines = append(lines, a.SHA256+"  "+filepath.Base(a.URL))
	}
	sort.Strings(lines)
	return os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

func archFromPath(path string) string {
	base := strings.TrimSuffix(path, ".tar.gz")
	idx := strings.LastIndex(base, "-linux-")
	if idx < 0 {
		return ""
	}
	return base[idx+len("-linux-"):]
}

func releaseTime(raw string) (time.Time, error) {
	if raw != "" {
		when, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return time.Time{}, fmt.Errorf("created-at must be RFC3339: %w", err)
		}
		return when.UTC(), nil
	}
	if epoch := os.Getenv("SOURCE_DATE_EPOCH"); epoch != "" {
		seconds, err := strconv.ParseInt(epoch, 10, 64)
		if err != nil || seconds < 0 {
			return time.Time{}, errors.New("SOURCE_DATE_EPOCH must be a non-negative Unix timestamp")
		}
		return time.Unix(seconds, 0).UTC(), nil
	}
	cmd := exec.Command("git", "log", "-1", "--format=%ct", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		return time.Time{}, fmt.Errorf("determine source timestamp: %w", err)
	}
	seconds, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse source timestamp: %w", err)
	}
	return time.Unix(seconds, 0).UTC(), nil
}

func validVersion(v string) bool {
	parts := strings.SplitN(v, "-", 2)
	nums := strings.Split(parts[0], ".")
	if len(nums) != 3 {
		return false
	}
	for _, n := range nums {
		if n == "" || (len(n) > 1 && n[0] == '0') {
			return false
		}
		if _, err := strconv.ParseUint(n, 10, 64); err != nil {
			return false
		}
	}
	return len(parts) == 1 || parts[1] != ""
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
