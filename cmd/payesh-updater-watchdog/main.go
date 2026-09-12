// Command payesh-updater-watchdog is intentionally separate from the Payesh
// application. It owns no network listener and only recovers activation
// transactions recorded in the root-owned external journal.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/Real-kia/payesh/internal/updater"
)

func main() {
	flags := flag.NewFlagSet("payesh-updater-watchdog", flag.ExitOnError)
	journal := flags.String("journal", "/var/lib/payesh/update/journal.json", "root-owned activation journal")
	interval := flags.Duration("interval", updater.DefaultWatchdogInterval, "recovery scan interval (1s..1h)")
	once := flags.Bool("once", false, "perform one recovery scan and exit")
	_ = flags.Parse(os.Args[1:])

	watchdog := updater.Watchdog{JournalPath: *journal, Interval: *interval, Logf: log.Printf}
	if *once {
		if err := watchdog.CheckOnce(); err != nil {
			fatal(err)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := watchdog.Run(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "payesh-updater-watchdog:", err)
	os.Exit(1)
}
