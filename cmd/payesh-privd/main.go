package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/Real-kia/payesh/internal/privd"
)

func main() {
	socket := flag.String("socket", envOr("PAYESH_PRIVD_SOCKET", privd.DefaultSocketPath), "permission-controlled helper Unix socket")
	moduleRoot := flag.String("module-root", envOr("PAYESH_MODULE_ROOT", "/var/lib/payesh/modules"), "root containing activated module releases")
	flag.Parse()
	modules, err := configuredModules()
	if err != nil {
		fatal(err)
	}
	server, err := privd.NewServer(privd.Config{SocketPath: *socket, ModuleRoot: *moduleRoot, Modules: modules})
	if err != nil {
		fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := server.ListenAndServe(ctx); err != nil && ctx.Err() == nil {
		fatal(err)
	}
}

func configuredModules() (map[string]privd.ModuleSpec, error) {
	// Unit names are fixed templates; callers can only select the curated
	// module ID and never provide a service command or path.
	return map[string]privd.ModuleSpec{
		"bandwidth-controls": {UnitTemplate: "payesh-bandwidth-module@%s.service", BinaryName: "bandwidth-controls"},
		"cpu-controls":       {UnitTemplate: "payesh-cpu-controls@%s.service", BinaryName: "cpu-controls"},
		"port-traffic":       {UnitTemplate: "payesh-port-traffic@%s.service", BinaryName: "port-traffic"},
	}, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" && filepath.IsAbs(value) && filepath.Clean(value) == value {
		return value
	}
	return fallback
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "payesh-privd:", err)
	os.Exit(1)
}
