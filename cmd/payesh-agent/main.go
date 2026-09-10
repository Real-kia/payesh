package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Real-kia/payesh/internal/collector"
	"github.com/Real-kia/payesh/internal/contracts"
)

func main() {
	root := flag.String("root", "/", "filesystem root containing proc-like host files")
	serverID := flag.String("server-id", "", "stable server id (16..128 URL-safe characters); omitted values use the persisted installation identity")
	identityFile := flag.String("identity-file", "/var/lib/payesh/server-id", "path for the persisted installation server id")
	epoch := flag.String("epoch", "", "collector epoch override; omitted values get a fresh epoch per start")
	billingInterfaces := flag.String("billing-interfaces", "", "comma-separated authoritative billing interfaces; empty uses safe discovery")
	interval := flag.Duration("interval", 0, "repeat collection interval; zero emits one sample")
	flag.Parse()

	collectorEpoch := contracts.CollectorEpoch(*epoch)
	if collectorEpoch == "" {
		collectorEpoch = collector.NewEpoch()
	}
	identity := contracts.ServerID(*serverID)
	if identity == "" {
		var err error
		identity, err = collector.LoadOrCreateServerID(*identityFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "server identity:", err)
			os.Exit(1)
		}
	}
	metricCollector := collector.NewCollector(*root, identity, collectorEpoch)
	metricCollector.AuthoritativeInterfaces = splitCSV(*billingInterfaces)
	if *interval != 0 && (*interval < 5*time.Second || *interval > time.Hour) {
		fmt.Fprintln(os.Stderr, "interval must be 0 (one sample) or between 5s and 1h")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	for {
		sample, err := metricCollector.Collect(ctx, time.Now().UTC())
		if err != nil {
			fmt.Fprintln(os.Stderr, "collect:", err)
			os.Exit(1)
		}
		if err := encoder.Encode(sample); err != nil {
			fmt.Fprintln(os.Stderr, "encode:", err)
			os.Exit(1)
		}
		if *interval <= 0 {
			return
		}
		timer := time.NewTimer(*interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func splitCSV(value string) []string {
	if value == "" {
		return nil
	}
	parts := make([]string, 0)
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			parts = append(parts, part)
		}
	}
	return parts
}
