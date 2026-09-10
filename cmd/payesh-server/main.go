package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Real-kia/payesh/internal/alerts"
	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/fleet"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/traffic"
)

func main() {
	dbPath := flag.String("db", "payesh.db", "SQLite database path")
	listen := flag.String("listen", "127.0.0.1:8787", "local listen address")
	token := flag.String("token", os.Getenv("PAYESH_LOCAL_TOKEN"), "Bearer token for protected local API (or PAYESH_LOCAL_TOKEN)")
	bootstrapSecret := flag.String("bootstrap-secret", os.Getenv("PAYESH_BOOTSTRAP_SECRET"), "one-time owner setup secret; enables browser-session API (or PAYESH_BOOTSTRAP_SECRET)")
	secureBrowserCookies := flag.Bool("secure-browser-cookies", false, "mark browser session cookies Secure; use when this HTTP listener is behind HTTPS")
	trustedProxyCIDRs := flag.String("trusted-proxy-cidrs", os.Getenv("PAYESH_TRUSTED_PROXY_CIDRS"), "comma-separated trusted reverse-proxy IPs/CIDRs for client-address headers")
	flag.Parse()
	if *bootstrapSecret != "" && !*secureBrowserCookies && !loopbackListenAddress(*listen) {
		fmt.Fprintln(os.Stderr, "browser-session API requires a loopback listener for HTTP cookies; use --secure-browser-cookies behind HTTPS")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	store, err := monitoring.OpenStore(ctx, *dbPath, monitoring.StoreOptions{MaxBytes: 512 << 20, ManagedPaths: []string{"/var/log/payesh"}})
	if err != nil {
		fmt.Fprintln(os.Stderr, "open store:", err)
		os.Exit(1)
	}
	alertEngine, alertErr := alerts.NewEngine(store)
	if alertErr != nil {
		fmt.Fprintln(os.Stderr, "create alert engine:", alertErr)
		os.Exit(1)
	}
	processor, processorErr := traffic.NewProcessor(store, alertEngine)
	if processorErr != nil {
		fmt.Fprintln(os.Stderr, "create package-05 processor:", processorErr)
		os.Exit(1)
	}
	store.SetIngestionObserver(processor.ObserveIngestion)
	// Notification credentials are owner-supplied process configuration and
	// stay in memory. Configure delivery before either API mode is selected so
	// bearer-token deployments receive the same alert notifications as browser
	// deployments.
	destinations := alertDestinationsFromEnvironment()
	var deliveryQueue *alerts.DeliveryQueue
	if len(destinations) > 0 {
		deliveryQueue = alerts.NewDeliveryQueue(alerts.Notifier{}, 2)
		alertEngine.ConfigureDelivery(deliveryQueue, func(context.Context, contracts.ServerID) []alerts.NotificationDestination {
			return append([]alerts.NotificationDestination(nil), destinations...)
		})
		defer deliveryQueue.Close()
	}
	// Derived traffic/alert work is durable but may be blocked behind a missing
	// sequence predecessor at startup. Keep startup bounded and let the worker
	// retry one bounded queue pass at a time; a deferred stream must not make
	// ready streams wait forever, nor should it make the server exit with raw
	// samples already safely committed.
	postProcessDone := processor.StartPostProcessRetry(ctx, traffic.DefaultPostProcessRetryInterval, func(err error) {
		if ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "retry pending sample processing:", err)
		}
	})
	retentionDone := make(chan struct{})
	go func() {
		defer close(retentionDone)
		prune := func() {
			pruneCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			now := time.Now().UTC()
			if refreshErr := store.RefreshServerStates(pruneCtx, now); refreshErr != nil && pruneCtx.Err() == nil {
				fmt.Fprintln(os.Stderr, "refresh server states:", refreshErr)
			}
			if livenessErr := evaluateUnreachable(pruneCtx, store, alertEngine, now); livenessErr != nil && pruneCtx.Err() == nil {
				fmt.Fprintln(os.Stderr, "evaluate node liveness:", livenessErr)
			}
			if _, pruneErr := store.Prune(pruneCtx, now, monitoring.DefaultRetentionPolicy()); pruneErr != nil && pruneCtx.Err() == nil {
				fmt.Fprintln(os.Stderr, "retention prune:", pruneErr)
			}
		}
		// Apply age limits once at startup, then keep the work bounded and
		// low-priority so normal API requests remain responsive.
		prune()
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				prune()
			}
		}
	}()
	defer func() {
		stop()
		<-postProcessDone
		<-retentionDone
		_ = store.Close()
	}()
	var handler http.Handler
	trafficService, trafficErr := traffic.NewService(store)
	if trafficErr != nil {
		fmt.Fprintln(os.Stderr, "create traffic service:", trafficErr)
		os.Exit(1)
	}
	if *bootstrapSecret != "" {
		alertService, alertErr := alerts.NewService(store)
		if alertErr != nil {
			fmt.Fprintln(os.Stderr, "create alert service:", alertErr)
			os.Exit(1)
		}
		alertService.Engine = alertEngine
		if starterErr := alertService.EnsureStarterRulesForServers(ctx); starterErr != nil {
			fmt.Fprintln(os.Stderr, "ensure starter alert rules:", starterErr)
			os.Exit(1)
		}
		// Traffic-generated alerts must share the same durable state machine and
		// delivery configuration as the rest of the server's alerts.
		trafficService.Alerts = alertService.Engine
		api, apiErr := fleet.NewAPIWithOptions(store, *bootstrapSecret, fleet.Options{SecureCookies: *secureBrowserCookies, TrustedProxyCIDRs: splitCommaList(*trustedProxyCIDRs), AlertService: alertService, TrafficService: trafficService})
		if apiErr != nil {
			fmt.Fprintln(os.Stderr, "create browser API:", apiErr)
			os.Exit(1)
		}
		handler = api.Handler()
	} else {
		api, apiErr := monitoring.NewAPI(store, *token)
		if apiErr != nil {
			fmt.Fprintln(os.Stderr, "create local API:", apiErr)
			os.Exit(1)
		}
		// Standalone bearer-token mode exposes the read-only forecast seam too;
		// allowance mutations remain a browser/CSRF operation.
		api.SetTrafficForecastHandler(trafficService.Handler())
		handler = api.Handler()
	}
	server := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// Live tails are bounded to five minutes by the API. Keep the write
		// deadline above that bound so an idle tail can still return its empty
		// bounded response instead of being closed by net/http first.
		WriteTimeout:   monitoring.MaxLiveTailDuration + time.Minute,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 16 << 10,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	fmt.Fprintf(os.Stderr, "payesh-server listening on %s\n", *listen)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}

func splitCommaList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	for index := range parts {
		parts[index] = strings.TrimSpace(parts[index])
	}
	return parts
}

func loopbackListenAddress(address string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func alertDestinationsFromEnvironment() []alerts.NotificationDestination {
	destinations := make([]alerts.NotificationDestination, 0, 2)
	if endpoint := strings.TrimSpace(os.Getenv("PAYESH_ALERT_WEBHOOK_URL")); endpoint != "" {
		destinations = append(destinations, alerts.NotificationDestination{Kind: "webhook", URL: endpoint, Secret: os.Getenv("PAYESH_ALERT_WEBHOOK_SECRET"), Enabled: true})
	}
	if token := os.Getenv("PAYESH_ALERT_TELEGRAM_TOKEN"); token != "" {
		apiURL := strings.TrimSpace(os.Getenv("PAYESH_ALERT_TELEGRAM_API_URL"))
		if apiURL == "" {
			apiURL = "https://api.telegram.org"
		}
		destinations = append(destinations, alerts.NotificationDestination{Kind: "telegram", URL: apiURL, Secret: token, ChatID: os.Getenv("PAYESH_ALERT_TELEGRAM_CHAT_ID"), Enabled: true})
	}
	return destinations
}

// evaluateUnreachable is the liveness side of the package-05 ingestion seam.
// RefreshServerStates persists the heartbeat-derived status; this bounded
// pass turns that status into node-unreachable alert observations so an
// economy-sampling node still gets availability transitions and grouped
// outage suppression.
func evaluateUnreachable(ctx context.Context, store *monitoring.Store, engine *alerts.Engine, at time.Time) error {
	if store == nil || engine == nil || at.IsZero() {
		return fmt.Errorf("liveness evaluator is unavailable")
	}
	cursor := ""
	for pageNumber := 0; pageNumber < 100; pageNumber++ {
		page, err := store.QueryServerPage(ctx, monitoring.MaxPageItems, cursor)
		if err != nil {
			return err
		}
		for _, server := range page.Items {
			// Servers can be enrolled after process startup. Ensure their editable
			// availability rule exists before evaluating the first timeout, even in
			// bearer-token mode where no browser alert service is constructed.
			if err := engine.EnsureStarterRules(ctx, server.ID); err != nil {
				return err
			}
			unreachable, err := store.IsServerUnreachable(ctx, server.ID)
			if err != nil {
				return err
			}
			if _, err := engine.EvaluateUnreachable(ctx, server.ID, at, unreachable); err != nil {
				return err
			}
		}
		if page.NextCursor == "" {
			return nil
		}
		cursor = page.NextCursor
	}
	return fmt.Errorf("server count exceeds liveness evaluation bound")
}
