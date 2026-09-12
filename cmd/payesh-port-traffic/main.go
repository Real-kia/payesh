package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/porttraffic"
)

func main() {
	dbPath := flag.String("db", "/var/lib/payesh/payesh.db", "Payesh SQLite database")
	serverID := flag.String("server-id", "", "local stable server identity")
	interval := flag.Duration("interval", porttraffic.DefaultRuntimeInterval, "reconciliation interval")
	flag.Parse()
	if strings.TrimSpace(*serverID) == "" {
		fatal("server-id is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := monitoring.OpenStore(ctx, *dbPath, monitoring.StoreOptions{})
	if err != nil {
		fatal(fmt.Sprintf("open store: %v", err))
	}
	defer store.Close()
	runtime, err := porttraffic.NewRuntime(porttraffic.RuntimeOptions{Store: store, ServerID: contracts.ServerID(strings.TrimSpace(*serverID)), Interval: *interval, OnError: func(err error) { fmt.Fprintln(os.Stderr, "port traffic:", err) }})
	if err != nil {
		fatal(err.Error())
	}
	if err := runtime.Tick(ctx, time.Now().UTC()); err != nil {
		fatal(err.Error())
	}
	done := runtime.Start(ctx)
	<-ctx.Done()
	if err := runtime.Stop(context.Background()); err != nil && !errors.Is(err, porttraffic.ErrOwnedTableMissing) {
		fatal(fmt.Sprintf("remove owned nftables table: %v", err))
	}
	<-done
}

func fatal(message string) {
	fmt.Fprintln(os.Stderr, "payesh-port-traffic:", message)
	os.Exit(1)
}
