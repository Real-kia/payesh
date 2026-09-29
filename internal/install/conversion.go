package install

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/Real-kia/payesh/internal/transport"
	_ "modernc.org/sqlite"
)

// ConversionConfig is supplied by an operator after enrolling this host with
// the destination hub. No authority is transferred by the installer itself.
type ConversionConfig struct {
	FromRole         string
	TransportURL     string
	NodeIdentityFile string
	HubCAFile        string
}

func CheckHubToNode(ctx context.Context, root string, cfg ConversionConfig) error {
	if cfg.FromRole != "hub" && cfg.FromRole != "standalone" {
		return errors.New("conversion source must be hub or standalone")
	}
	if root == "" {
		root = "/"
	}
	state, err := loadState(rooted(root, filepath.Join(dataDir, "install-state.json")))
	if err != nil {
		return fmt.Errorf("read installed role: %w", err)
	}
	if state.Role != cfg.FromRole {
		return fmt.Errorf("installed role is %q, not %q", state.Role, cfg.FromRole)
	}
	u, err := url.Parse(cfg.TransportURL)
	if err != nil || u.Scheme != "wss" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("destination hub transport URL must be wss:// without credentials or fragment")
	}
	if err := validateArtifactPath(cfg.NodeIdentityFile, false); err != nil {
		return fmt.Errorf("node identity file: %w", err)
	}
	if err := validateArtifactPath(cfg.HubCAFile, false); err != nil {
		return fmt.Errorf("hub CA file: %w", err)
	}
	node, err := transport.LoadNodeIdentity(cfg.NodeIdentityFile)
	if err != nil {
		return fmt.Errorf("load enrolled node identity: %w", err)
	}
	caBytes, err := os.ReadFile(cfg.HubCAFile)
	if err != nil {
		return err
	}
	block, _ := pem.Decode(caBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return errors.New("hub CA file is not a PEM certificate")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return fmt.Errorf("parse hub CA: %w", err)
	}
	keyPair, err := tls.X509KeyPair(node.CertificatePEM, node.PrivateKeyPEM)
	if err != nil {
		return fmt.Errorf("invalid node key pair: %w", err)
	}
	nodeCert, err := x509.ParseCertificate(keyPair.Certificate[0])
	if err != nil {
		return fmt.Errorf("parse node certificate: %w", err)
	}
	if nodeCert.Subject.CommonName != string(node.ServerID) {
		return errors.New("node identity does not match certificate")
	}
	dbPath := rooted(root, filepath.Join(dataDir, "payesh.db"))
	info, err := os.Lstat(dbPath)
	if err != nil {
		return fmt.Errorf("inspect hub database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("hub database must be a regular file")
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	var nodes int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM servers WHERE role='node'`).Scan(&nodes); err != nil {
		return fmt.Errorf("inspect managed nodes: %w", err)
	}
	if nodes > 0 {
		return fmt.Errorf("hub still manages %d node(s); migrate them to another hub and remove their old records first", nodes)
	}
	var identity []byte
	err = db.QueryRowContext(ctx, `SELECT state_json FROM fleet_identity_state WHERE singleton=1`).Scan(&identity)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("inspect node authority: %w", err)
	}
	if err == nil {
		var authority struct {
			Certificates map[string]string `json:"certificates"`
		}
		if err := json.Unmarshal(identity, &authority); err != nil {
			return fmt.Errorf("invalid node authority: %w", err)
		}
		if len(authority.Certificates) > 0 {
			return errors.New("hub still has active node certificates; migrate and revoke them first")
		}
	}
	return nil
}

func writeNodeConversionConfig(root string, cfg ConversionConfig, account Account) error {
	identityPath := rooted(root, filepath.Join(dataDir, "node-identity.json"))
	caPath := rooted(root, filepath.Join(dataDir, "hub-ca.pem"))
	for _, file := range []struct {
		source, destination string
		mode                os.FileMode
	}{{cfg.NodeIdentityFile, identityPath, 0o600}, {cfg.HubCAFile, caPath, 0o644}} {
		data, err := os.ReadFile(file.source)
		if err != nil {
			return err
		}
		if err := writeAtomic(file.destination, data, file.mode); err != nil {
			return err
		}
		if root == "/" && account.UID >= 0 {
			if err := os.Chown(file.destination, account.UID, account.GID); err != nil {
				return err
			}
		}
	}
	envPath := rooted(root, filepath.Join(configDir, "payesh.env"))
	data, err := os.ReadFile(envPath)
	if err != nil {
		return err
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if !strings.HasPrefix(line, "PAYESH_TRANSPORT_URL=") {
			lines = append(lines, line)
		}
	}
	lines = append(lines, "PAYESH_TRANSPORT_URL="+cfg.TransportURL)
	if err := writeAtomic(envPath, []byte(strings.Join(lines, "\n")+"\n"), 0o640); err != nil {
		return err
	}
	if root == "/" && account.UID >= 0 {
		return os.Chown(envPath, account.UID, account.GID)
	}
	return nil
}
