package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/Real-kia/payesh/internal/processmonitor"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

func main() {
	socket := flag.String("socket", "", "private Unix socket")
	serverID := flag.String("server-id", "", "local server identity")
	proc := flag.String("proc-root", "/proc", "Linux proc filesystem")
	check := flag.Bool("check", false, "verify proc access without starting a service")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	collector := processmonitor.NewCollector(*proc)
	if *check {
		_, e := collector.Sample(ctx, time.Now())
		if e != nil {
			fatal(e)
		}
		return
	}
	if !filepath.IsAbs(*socket) || filepath.Clean(*socket) != *socket || !regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`).MatchString(*serverID) {
		fatal(fmt.Errorf("invalid socket or server identity"))
	}
	r := &processmonitor.Runtime{Collector: collector, ServerID: *serverID}
	if e := r.Serve(ctx, *socket); e != nil {
		fatal(e)
	}
}
func fatal(e error) { fmt.Fprintln(os.Stderr, "process-monitoring:", e); os.Exit(1) }
