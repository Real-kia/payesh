package install

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ArtifactDownload is a job-scoped archive served by the hub. Pins are sent
// over the verified SSH connection, independently of HTTP delivery.
type ArtifactDownload struct {
	URL     string
	SHA256  string
	Digests map[string]string
	Close   func()
}

type downloadLease struct {
	path    string
	expires time.Time
}
type DownloadRegistry struct {
	mu     sync.Mutex
	leases map[string]downloadLease
	Now    func() time.Time
}

func (r *DownloadRegistry) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func SelectLinuxArtifacts(installer string, paths map[string]string, matrixDir, arch, role string, verify func(string, string) error) (map[string]string, error) {
	if arch != "amd64" && arch != "arm64" {
		return nil, errors.New("node architecture is not supported")
	}
	selected := make(map[string]string)
	for _, name := range append([]string{"payesh-install"}, requiredArtifacts(role)...) {
		path := paths[name]
		if name == "payesh-install" {
			path = installer
		}
		if name != "web-assets" {
			candidate := filepath.Join(matrixDir, name+"-linux-"+arch)
			if _, err := os.Stat(candidate); err == nil {
				path = candidate
			}
			if err := validateArtifactPath(path, false); err != nil {
				return nil, err
			}
			binary, err := elf.Open(path)
			if err != nil {
				return nil, fmt.Errorf("hub has no Linux/%s build of %s; update the hub's installation files", arch, name)
			}
			machine := binary.Machine
			binary.Close()
			if (arch == "amd64" && machine != elf.EM_X86_64) || (arch == "arm64" && machine != elf.EM_AARCH64) {
				return nil, fmt.Errorf("hub has no Linux/%s build of %s; update the hub's installation files", arch, name)
			}
		} else if err := validateArtifactPath(path, true); err != nil {
			return nil, err
		}
		if verify == nil {
			return nil, ErrArtifactVerifierRequired
		}
		if err := verify(name, path); err != nil {
			return nil, fmt.Errorf("verify hub artifact %s: %w", name, err)
		}
		selected[name] = path
	}
	return selected, nil
}

func (r *DownloadRegistry) Publish(ctx context.Context, baseURL, role string, paths map[string]string) (ArtifactDownload, error) {
	var result ArtifactDownload
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return result, errors.New("hub download URL must be an HTTP(S) origin without credentials")
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return result, err
	}
	token := hex.EncodeToString(random)
	dir, err := os.MkdirTemp("", "payesh-download-")
	if err != nil {
		return result, err
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(dir)
		}
	}()
	file, err := os.Create(filepath.Join(dir, "bundle.tar.gz"))
	if err != nil {
		return result, err
	}
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	result.Digests = make(map[string]string)
	writeErr := func() error {
		for _, name := range append([]string{"payesh-install"}, requiredArtifacts(role)...) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			path := paths[name]
			digest, err := ArtifactDigest(path, name == "web-assets")
			if err != nil {
				return err
			}
			result.Digests[name] = digest
			if name == "web-assets" {
				err = filepath.WalkDir(path, func(source string, entry os.DirEntry, walkErr error) error {
					if walkErr != nil {
						return walkErr
					}
					if entry.IsDir() {
						return nil
					}
					relative, err := filepath.Rel(path, source)
					if err != nil {
						return err
					}
					return writeDownloadFile(tw, source, "web-assets/"+filepath.ToSlash(relative), 0644)
				})
			} else {
				err = writeDownloadFile(tw, path, name, 0755)
			}
			if err != nil {
				return err
			}
		}
		return nil
	}()
	tarErr := tw.Close()
	gzErr := gz.Close()
	fileErr := file.Close()
	if err := errors.Join(writeErr, tarErr, gzErr, fileErr); err != nil {
		return result, err
	}
	info, err := os.Stat(file.Name())
	if err != nil {
		return result, err
	}
	if info.Size() > 128<<20 {
		return result, errors.New("installation bundle exceeds its download size limit")
	}
	sumFile, err := os.Open(file.Name())
	if err != nil {
		return result, err
	}
	sum := sha256.New()
	_, err = io.Copy(sum, sumFile)
	sumFile.Close()
	if err != nil {
		return result, err
	}
	result.SHA256 = hex.EncodeToString(sum.Sum(nil))
	r.mu.Lock()
	if r.leases == nil {
		r.leases = make(map[string]downloadLease)
	}
	for key, lease := range r.leases {
		if !lease.expires.After(r.now()) {
			os.RemoveAll(filepath.Dir(lease.path))
			delete(r.leases, key)
		}
	}
	if len(r.leases) >= 64 {
		r.mu.Unlock()
		return result, errors.New("hub download capacity is full")
	}
	r.leases[token] = downloadLease{path: file.Name(), expires: r.now().Add(30 * time.Minute)}
	r.mu.Unlock()
	result.URL = strings.TrimRight(baseURL, "/") + "/api/v1/install-artifacts/" + token + "/bundle.tar.gz"
	var once sync.Once
	result.Close = func() { once.Do(func() { r.mu.Lock(); delete(r.leases, token); r.mu.Unlock(); os.RemoveAll(dir) }) }
	keep = true
	return result, nil
}

func writeDownloadFile(tw *tar.Writer, source, name string, mode int64) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("installation bundle contains a non-regular file")
	}
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: info.Size(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err = io.Copy(tw, file)
	return err
}

func (r *DownloadRegistry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(req.URL.Path, "/api/v1/install-artifacts/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || len(parts[0]) != 64 || parts[1] != "bundle.tar.gz" {
		http.NotFound(w, req)
		return
	}
	r.mu.Lock()
	lease, ok := r.leases[parts[0]]
	valid := ok && lease.expires.After(r.now())
	r.mu.Unlock()
	if !valid {
		http.NotFound(w, req)
		return
	}
	file, err := os.Open(lease.path)
	if err != nil {
		http.NotFound(w, req)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="payesh-install.tar.gz"`)
	http.ServeContent(w, req, "payesh-install.tar.gz", info.ModTime(), file)
}

func artifactDownloadScript(dir, role, version, arch string, download ArtifactDownload) (string, error) {
	u, err := url.Parse(download.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return "", errors.New("invalid hub artifact download URL")
	}
	validDigest := func(value string) bool {
		decoded, err := hex.DecodeString(value)
		return err == nil && len(decoded) == 32 && value == strings.ToLower(value)
	}
	if !validDigest(download.SHA256) {
		return "", errors.New("invalid hub bundle checksum")
	}
	names := append([]string{"payesh-install"}, requiredArtifacts(role)...)
	for _, name := range names {
		if !validDigest(download.Digests[name]) {
			return "", fmt.Errorf("missing artifact checksum for %s", name)
		}
	}
	script := "set -eu\ncd " + shellQuote(dir) + `
fetch() {
 if command -v curl >/dev/null 2>&1; then curl -fsSL --connect-timeout 10 --max-time 120 --retry 1 "$1" -o "$2"
 elif command -v wget >/dev/null 2>&1; then wget -q -T 60 -t 2 -O "$2" "$1"
 else echo 'curl or wget is required' >&2; return 1; fi
}
checksum() {
 if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
 elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
 else openssl dgst -sha256 "$1" | awk '{print $NF}'; fi
}
source=hub
`
	// Download individual GitHub archives only when every pin can be checked.
	// tar -O streams a named member without extracting arbitrary archive paths.
	if role == "node" || role == "cli-only" {
		if regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(version) {
			script += "github_ok=1\n"
			for _, name := range names {
				remote := "https://github.com/Real-kia/payesh/releases/download/v" + version + "/" + name + "-linux-" + arch + ".tar.gz"
				script += "if [ \"$github_ok\" = 1 ]; then\n if fetch " + shellQuote(remote) + " " + shellQuote(name+".download") + " && tar -xzOf " + shellQuote(name+".download") + " " + shellQuote(name) + " > " + shellQuote(name+".tmp") + " && [ \"$(checksum " + shellQuote(name+".tmp") + ")\" = " + shellQuote(download.Digests[name]) + " ]; then mv -f " + shellQuote(name+".tmp") + " " + shellQuote(name) + "; else github_ok=0; fi\nfi\n"
			}
			script += "if [ \"$github_ok\" = 1 ]; then source=github; fi\n"
		}
	}
	script += "if [ \"$source\" = hub ]; then\n fetch " + shellQuote(download.URL) + " bundle.download\n [ \"$(checksum bundle.download)\" = " + shellQuote(download.SHA256) + " ] || { echo 'Hub bundle checksum mismatch' >&2; exit 1; }\n tar -xzf bundle.download\nfi\n"
	for _, name := range names {
		if name != "web-assets" {
			script += "[ \"$(checksum " + shellQuote(name) + ")\" = " + shellQuote(download.Digests[name]) + " ] || exit 1\nchmod 700 " + shellQuote(name) + "\n"
		}
	}
	script += "rm -f -- *.download *.tmp\nprintf 'download_source=%s\\n' \"$source\"\n"
	return script, nil
}
