package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Real-kia/payesh/internal/bandwidth"
	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

func main() {
	dbPath := flag.String("db", "/var/lib/payesh/payesh.db", "Payesh SQLite database")
	serverID := flag.String("server-id", "", "local stable server identity")
	serverIDFile := flag.String("server-id-file", "/var/lib/payesh/server-id", "existing local stable server identity file")
	stateDir := flag.String("state-dir", "/var/lib/payesh/bandwidth", "local module state")
	socketPath := flag.String("socket", "/run/payesh/bandwidth.sock", "permission-controlled module HTTP socket")
	confirmURL := flag.String("management-confirm-url", "", "trusted local configuration URL used for post-apply management round trip")
	remotePorts := flag.String("payesh-remote-ports", "443", "comma-separated remote Payesh hub ports to exempt")
	localPorts := flag.String("payesh-local-ports", "8787", "comma-separated local Payesh web ports to exempt")
	watchdogOnly := flag.Bool("watchdog-only", false, "run only the independent rollback watchdog")
	watchdogInterval := flag.Duration("watchdog-interval", 5*time.Second, "rollback scan interval (1s..30s)")
	flag.Parse()
	if *watchdogOnly {
		runWatchdog(*stateDir, *watchdogInterval)
		return
	}
	resolvedServerID := strings.TrimSpace(*serverID)
	if resolvedServerID == "" {
		data, readErr := os.ReadFile(*serverIDFile)
		if readErr != nil {
			fatal(fmt.Sprintf("read server identity: %v", readErr))
		}
		resolvedServerID = strings.TrimSpace(string(data))
	}
	if len(resolvedServerID) < 16 || len(resolvedServerID) > 128 || !filepath.IsAbs(*dbPath) || !filepath.IsAbs(*stateDir) || !filepath.IsAbs(*socketPath) {
		fatal("invalid bandwidth module configuration")
	}
	confirmation, err := managementConfirmation(*confirmURL)
	if err != nil {
		fatal(err.Error())
	}
	remote, err := parsePorts(*remotePorts)
	if err != nil {
		fatal("invalid remote management ports")
	}
	local, err := parsePorts(*localPorts)
	if err != nil {
		fatal("invalid local management ports")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := monitoring.OpenStore(ctx, *dbPath, monitoring.StoreOptions{})
	if err != nil {
		fatal(fmt.Sprintf("open store: %v", err))
	}
	defer store.Close()
	runtime, err := bandwidth.NewRuntime(bandwidth.RuntimeOptions{Store: store, ServerID: contracts.ServerID(resolvedServerID), StateDir: *stateDir, PayeshRemotePorts: remote, PayeshLocalPorts: local, ManagementRoundTrip: confirmation})
	if err != nil {
		fatal(err.Error())
	}
	listener, err := listenUnix(*socketPath)
	if err != nil {
		fatal(fmt.Sprintf("listen: %v", err))
	}
	defer func() { listener.Close(); _ = os.Remove(*socketPath) }()
	enforcerDone := runtime.Enforcer.Start(ctx)
	server := &http.Server{Handler: runtime.Service.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	err = server.Serve(listener)
	stop()
	<-enforcerDone
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal(fmt.Sprintf("serve: %v", err))
	}
}

func managementConfirmation(raw string) (func(context.Context) error, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname()))) || parsed.User != nil {
		return nil, errors.New("management-confirm-url must be HTTPS or loopback HTTP")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	return func(ctx context.Context) error {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode > 299 {
			return fmt.Errorf("management endpoint returned %d", response.StatusCode)
		}
		return nil
	}, nil
}

func listenUnix(path string) (net.Listener, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("socket path exists and is not a socket")
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		listener.Close()
		return nil, err
	}
	return listener, nil
}

func parsePorts(value string) ([]uint16, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	parts := strings.Split(value, ",")
	if len(parts) > 32 {
		return nil, errors.New("too many ports")
	}
	ports := make([]uint16, 0, len(parts))
	for _, part := range parts {
		value, err := strconv.ParseUint(strings.TrimSpace(part), 10, 16)
		if err != nil || value == 0 {
			return nil, errors.New("invalid port")
		}
		ports = append(ports, uint16(value))
	}
	return ports, nil
}
func isLoopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}
func fatal(message string) { fmt.Fprintln(os.Stderr, message); os.Exit(1) }

func runWatchdog(stateDir string, interval time.Duration) {
	if !filepath.IsAbs(stateDir) || interval < time.Second || interval > 30*time.Second {
		fatal("invalid watchdog configuration")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	guard := bandwidth.FileGuard{Dir: filepath.Join(stateDir, "rollback")}
	kernel := &bandwidth.TCBackend{StateDir: filepath.Join(stateDir, "tc")}
	recover := func() {
		recovered, err := guard.RecoverDue(ctx, kernel, time.Now().UTC())
		for _, id := range recovered {
			fmt.Fprintf(os.Stderr, "recovered expired bandwidth checkpoint %s\n", id)
		}
		if err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "bandwidth recovery:", err)
		}
	}
	recover()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			recover()
		}
	}
}
