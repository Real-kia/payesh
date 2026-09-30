// process-package signs an offline-built optional package. No private key is
// generated, committed, or passed into the GitHub release workflow.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/modules"
	"github.com/Real-kia/payesh/internal/release"
	"github.com/Real-kia/payesh/internal/trust"
	"os"
	"path/filepath"
	"time"
)

func main() {
	dir := flag.String("out", "", "new output directory")
	binaries := flag.String("binaries", "dist/matrix", "cross-built binaries directory")
	key := flag.String("key", "", "operator-owned Ed25519 key file")
	flag.Parse()
	if e := run(*dir, *binaries, *key); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run(dir, binaries, keyFile string) error {
	private, e := release.ReadPrivateKey(keyFile)
	if e != nil {
		return e
	}
	defer func() {
		for i := range private {
			private[i] = 0
		}
	}()
	public := private.Public().(ed25519.PublicKey)
	if string(public) != string(modules.ProcessTrustAnchor().PublicKey) {
		return errors.New("signing key does not match the pinned process package anchor")
	}
	if dir == "" {
		return errors.New("output directory is required")
	}
	if e := os.Mkdir(dir, 0700); e != nil {
		return e
	}
	success := false
	defer func() {
		if !success {
			os.RemoveAll(dir)
		}
	}()
	when := time.Now().UTC().Truncate(time.Second)
	for _, arch := range []string{"amd64", "arm64"} {
		path := filepath.Join(binaries, "process-monitoring-linux-"+arch)
		binary, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		executable, e := elf.Open(path)
		if e != nil {
			return e
		}
		machine := executable.Machine
		executable.Close()
		if arch == "amd64" && machine != elf.EM_X86_64 || arch == "arm64" && machine != elf.EM_AARCH64 {
			return errors.New("package architecture mismatch")
		}
		name := "process-monitoring-linux-" + arch
		archivePath := filepath.Join(dir, name+".tar.gz")
		f, e := os.OpenFile(archivePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		gz := gzip.NewWriter(f)
		tw := tar.NewWriter(gz)
		if e := tw.WriteHeader(&tar.Header{Name: "bin/process-monitoring", Mode: 0755, Size: int64(len(binary)), ModTime: when, Typeflag: tar.TypeReg}); e != nil {
			f.Close()
			return e
		}
		if _, e := tw.Write(binary); e != nil {
			f.Close()
			return e
		}
		if e := tw.Close(); e != nil {
			f.Close()
			return e
		}
		if e := gz.Close(); e != nil {
			f.Close()
			return e
		}
		if e := f.Close(); e != nil {
			return e
		}
		archive, e := os.ReadFile(archivePath)
		if e != nil {
			return e
		}
		digest := sha256.Sum256(archive)
		manifest := contracts.ModuleManifest{Format: contracts.ModuleManifestFormat, ModuleID: modules.ProcessModuleID, ModuleVersion: "0.1.0", MinCore: "0.2.11", ProtocolMin: "v1", ProtocolMax: "v1", OS: "linux", Architecture: arch, CompressedBytes: uint64(len(archive)), UnpackedBytes: uint64(len(binary)), SHA256: hex.EncodeToString(digest[:]), SigningKeyID: modules.ProcessSigningKeyID, CreatedAt: when}
		if e := manifest.Validate(); e != nil {
			return e
		}
		_, signature, e := trust.Sign(private, manifest)
		if e != nil {
			return e
		}
		body, e := json.MarshalIndent(manifest, "", "  ")
		if e != nil {
			return e
		}
		if e := os.WriteFile(filepath.Join(dir, name+".manifest.json"), append(body, '\n'), 0600); e != nil {
			return e
		}
		if e := os.WriteFile(filepath.Join(dir, name+".manifest.sig"), []byte(signature+"\n"), 0600); e != nil {
			return e
		}
	}
	success = true
	return nil
}
