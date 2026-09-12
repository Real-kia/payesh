package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/cpucontrol"
	"github.com/Real-kia/payesh/internal/monitoring"
)

func main() {
	dbPath := flag.String("db", "/var/lib/payesh/payesh.db", "Payesh SQLite database")
	serverID := flag.String("server-id", "", "local stable server identity")
	serverIDFile := flag.String("server-id-file", "/var/lib/payesh/server-id", "existing local stable server identity file")
	cgroupRoot := flag.String("cgroup-root", cpucontrol.DefaultCgroupRoot, "cgroup v2 mount point")
	stateRoot := flag.String("state-dir", cpucontrol.DefaultCPUStateRoot, "CPU module state directory")
	socketPath := flag.String("socket", cpucontrol.DefaultCPUSocket, "permission-controlled module Unix socket")
	authToken := flag.String("auth-token", os.Getenv("PAYESH_CPU_MODULE_TOKEN"), "optional module proxy authentication token")
	authTokenFile := flag.String("auth-token-file", "", "file containing the module proxy authentication token")
	flag.Parse()

	resolvedServerID := strings.TrimSpace(*serverID)
	if resolvedServerID == "" {
		data, err := os.ReadFile(*serverIDFile)
		if err != nil {
			fatal(fmt.Sprintf("read server identity: %v", err))
		}
		resolvedServerID = strings.TrimSpace(string(data))
	}
	if len(resolvedServerID) < 16 || len(resolvedServerID) > 128 || !filepath.IsAbs(*dbPath) || !filepath.IsAbs(*cgroupRoot) || !filepath.IsAbs(*stateRoot) || !filepath.IsAbs(*socketPath) {
		fatal("invalid CPU module configuration")
	}
	if *authTokenFile != "" {
		if !filepath.IsAbs(*authTokenFile) || filepath.Clean(*authTokenFile) != *authTokenFile {
			fatal("auth-token-file must be a clean absolute path")
		}
		data, err := os.ReadFile(*authTokenFile)
		if err != nil {
			fatal(fmt.Sprintf("read auth token: %v", err))
		}
		*authToken = strings.TrimSpace(string(data))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := monitoring.OpenStore(ctx, *dbPath, monitoring.StoreOptions{})
	if err != nil {
		fatal(fmt.Sprintf("open store: %v", err))
	}
	defer store.Close()
	runtime, err := cpucontrol.NewRuntime(cpucontrol.RuntimeOptions{
		Store: store, ServerID: contracts.ServerID(resolvedServerID), CgroupRoot: *cgroupRoot,
		OwnershipRoot: filepath.Join(*stateRoot, "ownership"), SocketPath: *socketPath, AuthToken: strings.TrimSpace(*authToken),
	})
	if err != nil {
		fatal(err.Error())
	}
	if err := runtime.ListenAndServe(ctx); err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
		fatal(fmt.Sprintf("serve CPU module: %v", err))
	}
}

func fatal(message string) {
	fmt.Fprintln(os.Stderr, "payesh-cpu-module:", message)
	os.Exit(1)
}
