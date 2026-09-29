package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Real-kia/payesh/internal/webtls"
)

// domain sets, shows, or removes the dashboard domain and its automatic
// HTTPS certificate. It runs the ACME request itself and then restarts
// payesh-server so the new certificate is served on the dashboard port.
func domain(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("domain", flag.ContinueOnError)
	dir := flags.String("tls-dir", webtls.DefaultDir, "automatic HTTPS state directory")
	email := flags.String("email", "", "optional email for Let's Encrypt expiry notices")
	cloudflareToken := flags.String("cloudflare-token", "", "Cloudflare API token (Zone:DNS:Edit) for DNS validation when port 80 is in use; or PAYESH_CLOUDFLARE_API_TOKEN")
	remove := flags.Bool("remove", false, "remove the domain and go back to plain HTTP")
	restart := flags.Bool("restart", true, "restart payesh-server after a change")
	flags.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage:
  payesh domain                         show the current domain and certificate
  payesh domain panel.example.com       get a certificate and switch the dashboard to HTTPS
  payesh domain --remove                remove the domain and go back to HTTP

options:`)
		flags.PrintDefaults()
	}
	// Accept the domain before or after the options.
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if name == "" && flags.NArg() > 0 {
		name = flags.Arg(0)
	}
	manager, err := webtls.NewManager(*dir)
	if err != nil {
		return err
	}
	if *remove {
		if err := manager.Disable(); err != nil {
			return err
		}
		fmt.Println("Domain removed. The dashboard is back on plain HTTP (not encrypted).")
		return restartServer(*restart)
	}
	if name == "" {
		printDomainStatus(manager.Status())
		return nil
	}
	token := *cloudflareToken
	if token == "" {
		token = os.Getenv("PAYESH_CLOUDFLARE_API_TOKEN")
	}
	fmt.Printf("Requesting a Let's Encrypt certificate for %s ...\n", name)
	if err := manager.Configure(ctx, webtls.Config{Domain: name, Email: *email, CloudflareToken: token}); err != nil {
		chownToService(*dir)
		if errors.Is(err, webtls.ErrPort80InUse) {
			return fmt.Errorf("%w\n\nRe-run with a Cloudflare token, for example:\n  sudo PAYESH_CLOUDFLARE_API_TOKEN=... payesh domain %s", err, name)
		}
		return err
	}
	chownToService(*dir)
	status := manager.Status()
	fmt.Printf("Certificate issued for %s (validated with %s).\n", status.Domain, status.Method)
	if err := restartServer(*restart); err != nil {
		return err
	}
	port := dashboardPort()
	fmt.Printf("\nDashboard: https://%s:%s\n", status.Domain, port)
	return nil
}

func printDomainStatus(status webtls.Status) {
	if status.Domain == "" {
		fmt.Println("No domain is set. The dashboard uses plain HTTP (not encrypted).")
		fmt.Println("Set one with: sudo payesh domain panel.example.com")
		return
	}
	fmt.Printf("domain:  %s\nstate:   %s\n", status.Domain, status.State)
	if status.Method != "" {
		fmt.Printf("method:  %s\n", status.Method)
	}
	if status.ExpiresAt != nil {
		fmt.Printf("expires: %s\n", status.ExpiresAt.Format("2006-01-02"))
	}
	if status.Error != "" {
		fmt.Printf("error:   %s\n", status.Error)
	}
}

// chownToService hands the TLS state to the payesh service account so the
// server can serve and renew the certificate.
func chownToService(dir string) {
	account, err := user.Lookup("payesh")
	if err != nil {
		return
	}
	uid, uidErr := strconv.Atoi(account.Uid)
	gid, gidErr := strconv.Atoi(account.Gid)
	if uidErr != nil || gidErr != nil {
		return
	}
	_ = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry.Type()&fs.ModeSymlink == 0 {
			_ = os.Lchown(path, uid, gid)
		}
		return nil
	})
}

func restartServer(enabled bool) error {
	if !enabled {
		return nil
	}
	var cmd *exec.Cmd
	switch {
	case commandExists("systemctl"):
		cmd = exec.Command("systemctl", "try-restart", "payesh-server.service")
	case commandExists("rc-service"):
		cmd = exec.Command("rc-service", "payesh-server", "restart")
	default:
		fmt.Println("Restart payesh-server to apply the change.")
		return nil
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("restart payesh-server: %w", err)
	}
	return nil
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// dashboardPort reads the installed service's -listen port.
func dashboardPort() string {
	for _, path := range []string{"/etc/systemd/system/payesh-server.service", "/etc/init.d/payesh-server"} {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, field := range strings.Fields(strings.NewReplacer(`"`, " ", `'`, " ").Replace(string(b))) {
			if value, ok := strings.CutPrefix(field, "-listen="); ok {
				if i := strings.LastIndex(value, ":"); i >= 0 {
					return value[i+1:]
				}
			}
		}
	}
	return "8787"
}
