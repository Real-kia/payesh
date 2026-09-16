package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Real-kia/payesh/internal/alerts"
	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/fleet"
	installpkg "github.com/Real-kia/payesh/internal/install"
	"github.com/Real-kia/payesh/internal/modules"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/porttraffic"
	"github.com/Real-kia/payesh/internal/privd"
	"github.com/Real-kia/payesh/internal/traffic"
	"github.com/Real-kia/payesh/internal/transport"
	"github.com/Real-kia/payesh/internal/trust"
	"github.com/Real-kia/payesh/internal/updater"
)

func main() {
	dbPath := flag.String("db", "payesh.db", "SQLite database path")
	listen := flag.String("listen", "127.0.0.1:8787", "local listen address")
	token := flag.String("token", os.Getenv("PAYESH_LOCAL_TOKEN"), "Bearer token for protected local API (or PAYESH_LOCAL_TOKEN)")
	bootstrapSecret := flag.String("bootstrap-secret", os.Getenv("PAYESH_BOOTSTRAP_SECRET"), "one-time owner setup secret; enables browser-session API (or PAYESH_BOOTSTRAP_SECRET)")
	secureCookiesDefault, secureCookiesErr := environmentBool("PAYESH_SECURE_BROWSER_COOKIES", false)
	if secureCookiesErr != nil {
		fmt.Fprintln(os.Stderr, "configure browser cookies:", secureCookiesErr)
		os.Exit(2)
	}
	secureBrowserCookies := flag.Bool("secure-browser-cookies", secureCookiesDefault, "mark browser session cookies Secure; use when this HTTP listener is behind HTTPS (or PAYESH_SECURE_BROWSER_COOKIES)")
	trustedProxyCIDRs := flag.String("trusted-proxy-cidrs", os.Getenv("PAYESH_TRUSTED_PROXY_CIDRS"), "comma-separated trusted reverse-proxy IPs/CIDRs for client-address headers")
	nodeListen := flag.String("node-listen", os.Getenv("PAYESH_NODE_LISTEN"), "optional TLS node transport listen address (disabled when empty)")
	nodeTLSCert := flag.String("node-tls-cert", os.Getenv("PAYESH_NODE_TLS_CERT"), "node transport TLS server certificate PEM path")
	nodeTLSKey := flag.String("node-tls-key", os.Getenv("PAYESH_NODE_TLS_KEY"), "node transport TLS server private key PEM path")
	transportURL := flag.String("transport-url", os.Getenv("PAYESH_TRANSPORT_URL"), "public transport URL advertised to nodes (or PAYESH_TRANSPORT_URL)")
	flag.Parse()
	nodeConfig, nodeConfigErr := newNodeTransportConfig(*nodeListen, *nodeTLSCert, *nodeTLSKey)
	if nodeConfigErr != nil {
		fmt.Fprintln(os.Stderr, "configure node transport:", nodeConfigErr)
		os.Exit(2)
	}
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
	var updateWorkerDone <-chan struct{}
	var installWorkerDone <-chan struct{}
	defer func() {
		stop()
		if updateWorkerDone != nil {
			<-updateWorkerDone
		}
		if installWorkerDone != nil {
			<-installWorkerDone
		}
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
	var enrollmentAuthority *transport.CertificateAuthority
	var nodeHub *transport.Hub
	var updateScheduler *updater.Scheduler
	var installService *fleet.InstallService
	if nodeConfig.Enabled() || *bootstrapSecret != "" {
		enrollmentAuthority, err = transport.NewPersistentCertificateAuthority(time.Now().UTC(), store)
		if err != nil {
			fmt.Fprintln(os.Stderr, "configure enrollment authority:", err)
			os.Exit(1)
		}
		nodeHub, err = transport.NewHub(enrollmentAuthority, store)
		if err != nil {
			fmt.Fprintln(os.Stderr, "configure node transport hub:", err)
			os.Exit(1)
		}
	}
	if *bootstrapSecret != "" {
		updateScheduler = updater.NewScheduler(store, nil)
		installService, err = fleet.NewInstallService(store)
		if err != nil {
			fmt.Fprintln(os.Stderr, "configure installation jobs:", err)
			os.Exit(1)
		}
		// Reuse the root-owned artifacts installed on the hub. An optional
		// artifact directory can override these paths for a staged release, but
		// a normal installer deployment requires no manual server configuration.
		paths := installedArtifactPaths(strings.TrimSpace(os.Getenv("PAYESH_INSTALL_ARTIFACT_DIR")))
		if installArtifactsReady(paths) {
			installService.InstallerPath = paths["payesh-install"]
			installService.Artifacts = paths
			installService.VerifyArtifact = func(name, path string) error {
				_, digestErr := installpkg.ArtifactDigest(path, name == "web-assets")
				return digestErr
			}
			installService.Executor = fleet.SSHInstallerFunc(installpkg.InstallOverSSH)
		}
		installService.Authority = enrollmentAuthority
		installService.TransportURL = strings.TrimSpace(*transportURL)
		if nodeConfig.CertFile != "" {
			if certPEM, err := os.ReadFile(nodeConfig.CertFile); err == nil {
				installService.HubTrustPEM = certPEM
			}
		}
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
		moduleRegistry, registryErr := moduleTrustRegistryFromEnvironment()
		if registryErr != nil {
			fmt.Fprintln(os.Stderr, "configure module trust registry:", registryErr)
			os.Exit(1)
		}
		moduleManager := &modules.Manager{Store: store, Trust: moduleRegistry, RootDir: moduleRootDir(), LocalServerID: contracts.ServerID(strings.TrimSpace(os.Getenv("PAYESH_SERVER_ID")))}
		if socket := strings.TrimSpace(os.Getenv("PAYESH_PRIVD_SOCKET")); socket != "" {
			moduleManager.Executor = privdModuleExecutor{client: privd.Client{SocketPath: socket}}
		}
		moduleService := modules.NewService(moduleManager)
		portTrafficService := porttraffic.NewService(&porttraffic.Manager{Store: store})
		var cpuControlService interface{ Handler() http.Handler } = unavailableCPUService{}
		if socket := strings.TrimSpace(os.Getenv("PAYESH_CPU_SOCKET")); socket != "" {
			var proxy http.Handler
			var proxyErr error
			if moduleToken := strings.TrimSpace(os.Getenv("PAYESH_CPU_MODULE_TOKEN")); moduleToken != "" {
				proxy, proxyErr = modules.NewAuthenticatedUnixModuleProxy(socket, moduleToken)
			} else {
				proxy, proxyErr = modules.NewUnixModuleProxy(socket)
			}
			if proxyErr != nil {
				fmt.Fprintln(os.Stderr, "configure CPU Controls module:", proxyErr)
				os.Exit(1)
			}
			cpuControlService = handlerService{handler: proxy}
		}
		var bandwidthService interface{ Handler() http.Handler }
		if socket := strings.TrimSpace(os.Getenv("PAYESH_BANDWIDTH_SOCKET")); socket != "" {
			proxy, proxyErr := modules.NewUnixModuleProxy(socket)
			if proxyErr != nil {
				fmt.Fprintln(os.Stderr, "configure bandwidth module:", proxyErr)
				os.Exit(1)
			}
			bandwidthService = handlerService{handler: proxy}
		}
		api, apiErr := fleet.NewAPIWithOptions(store, *bootstrapSecret, fleet.Options{SecureCookies: *secureBrowserCookies, TrustedProxyCIDRs: splitCommaList(*trustedProxyCIDRs), AlertService: alertService, TrafficService: trafficService, ModuleService: moduleService, CPUControlService: cpuControlService, BandwidthService: bandwidthService, PortTrafficService: portTrafficService, EnrollmentAuthority: enrollmentAuthority, UpdateScheduler: updateScheduler, InstallService: installService})
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
	if updateScheduler != nil {
		updateWorkerDone = updateScheduler.StartWorker(ctx, updater.DefaultWorkerInterval, func(workerErr error) {
			if ctx.Err() == nil {
				fmt.Fprintln(os.Stderr, "update worker:", workerErr)
			}
		})
	}
	if installService != nil {
		installWorkerDone = installService.StartWorker(ctx, 5*time.Second, func(workerErr error) {
			if ctx.Err() == nil {
				fmt.Fprintln(os.Stderr, "installation worker:", workerErr)
			}
		})
	}
	var nodeServer *http.Server
	var nodeListener net.Listener
	if nodeConfig.Enabled() {
		certificate, certErr := tls.LoadX509KeyPair(nodeConfig.CertFile, nodeConfig.KeyFile)
		if certErr != nil {
			fmt.Fprintln(os.Stderr, "load node transport TLS certificate:", certErr)
			os.Exit(1)
		}
		if nodeHub == nil {
			fmt.Fprintln(os.Stderr, "configure node transport hub: hub is unavailable")
			os.Exit(1)
		}
		nodeServer = &http.Server{
			Addr:              nodeConfig.Listen,
			Handler:           nodeTransportHandler(nodeHub),
			ReadHeaderTimeout: 5 * time.Second,
			MaxHeaderBytes:    16 << 10,
			TLSConfig:         nodeTransportTLSConfig(certificate),
		}
		nodeListener, err = net.Listen("tcp", nodeConfig.Listen)
		if err != nil {
			fmt.Fprintln(os.Stderr, "listen node transport:", err)
			os.Exit(1)
		}
		tlsListener := tls.NewListener(nodeListener, nodeServer.TLSConfig)
		nodeListener = tlsListener
		go func() {
			if serveErr := nodeServer.Serve(tlsListener); serveErr != nil && serveErr != http.ErrServerClosed && ctx.Err() == nil {
				fmt.Fprintln(os.Stderr, "node transport:", serveErr)
			}
		}()
		fmt.Fprintf(os.Stderr, "payesh node transport listening on %s\n", nodeConfig.Listen)
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
		if nodeServer != nil {
			_ = nodeServer.Shutdown(shutdownCtx)
		}
		_ = server.Shutdown(shutdownCtx)
	}()
	defer func() {
		if nodeListener != nil {
			_ = nodeListener.Close()
		}
	}()
	// Bind before announcing readiness. Apart from surfacing bind errors before
	// the process enters Serve, this makes :0 useful to disposable acceptance
	// runs: the log contains the kernel-selected, race-free endpoint.
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	defer listener.Close()
	fmt.Fprintf(os.Stderr, "payesh-server listening on %s\n", listener.Addr().String())
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}

func nodeTransportHandler(hub *transport.Hub) http.Handler {
	mux := http.NewServeMux()
	// The TLS handshake may omit a client certificate so a node can perform
	// its one-time bootstrap. The ordinary node route still rejects an
	// anonymous peer in WebSocketHandler before upgrading, and Hub.Open
	// remains authoritative for CA ownership/revocation checks.
	mux.Handle("/node/bootstrap/v1", transport.BootstrapWebSocketHandler(hub))
	mux.Handle("/node/v1", transport.WebSocketHandler(hub))
	return mux
}

func nodeTransportTLSConfig(certificate tls.Certificate) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
		// Bootstrap is server-authenticated before a node has a client
		// certificate. Route-level handlers enforce certificates for the
		// ordinary node channel.
		ClientAuth: tls.RequestClientCert,
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

type nodeTransportConfig struct {
	Listen   string
	CertFile string
	KeyFile  string
}

func (c nodeTransportConfig) Enabled() bool { return c.Listen != "" }

func newNodeTransportConfig(listen, certFile, keyFile string) (nodeTransportConfig, error) {
	c := nodeTransportConfig{Listen: strings.TrimSpace(listen), CertFile: strings.TrimSpace(certFile), KeyFile: strings.TrimSpace(keyFile)}
	if c.Listen == "" {
		if c.CertFile != "" || c.KeyFile != "" {
			return nodeTransportConfig{}, fmt.Errorf("node TLS certificate/key require --node-listen or PAYESH_NODE_LISTEN")
		}
		return c, nil
	}
	if c.CertFile == "" || c.KeyFile == "" {
		return nodeTransportConfig{}, fmt.Errorf("node transport requires both TLS certificate and key")
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return nodeTransportConfig{}, fmt.Errorf("node listen address is invalid: %w", err)
	}
	return c, nil
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

func environmentBool(name string, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	return parsed, nil
}

func installedArtifactPaths(artifactDir string) map[string]string {
	paths := map[string]string{
		"payesh":         "/usr/bin/payesh",
		"payesh-agent":   "/usr/bin/payesh-agent",
		"payesh-privd":   "/usr/bin/payesh-privd",
		"payesh-server":  "/usr/bin/payesh-server",
		"payesh-install": "/usr/bin/payesh-install",
		"web-assets":     "/usr/share/payesh/web-assets",
	}
	if artifactDir == "" {
		return paths
	}
	for name := range paths {
		paths[name] = filepath.Join(artifactDir, name)
	}
	return paths
}

func installArtifactsReady(paths map[string]string) bool {
	// Adding a server installs a node. Hub/standalone artifacts remain in the
	// map for explicit role requests, where the orchestrator reports the exact
	// missing artifact rather than disabling SSH installation altogether.
	for _, name := range []string{"payesh-install", "payesh", "payesh-agent", "payesh-privd"} {
		path := strings.TrimSpace(paths[name])
		if path == "" {
			return false
		}
		if _, err := installpkg.ArtifactDigest(path, false); err != nil {
			return false
		}
	}
	return true
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

// moduleTrustRegistryFromEnvironment builds the pinned module-signing trust
// anchor from PAYESH_MODULE_TRUST_KEY_ID/PAYESH_MODULE_TRUST_PUBLIC_KEY_B64
// (a raw unpadded-base64url Ed25519 public key). No production release-
// signing key is committed to this repository or generated here (see
// docs/contracts/RELEASE_FORMAT.md); an empty registry is intentional and
// safe: the catalog and status routes keep working, while every install
// request fails closed with "unknown signing key" until an operator
// provisions a real anchor.
func moduleTrustRegistryFromEnvironment() (*trust.Registry, error) {
	keyID := strings.TrimSpace(os.Getenv("PAYESH_MODULE_TRUST_KEY_ID"))
	publicKeyB64 := strings.TrimSpace(os.Getenv("PAYESH_MODULE_TRUST_PUBLIC_KEY_B64"))
	if keyID == "" || publicKeyB64 == "" {
		return trust.NewRegistry()
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(publicKeyB64)
	if err != nil {
		return nil, fmt.Errorf("PAYESH_MODULE_TRUST_PUBLIC_KEY_B64 is not valid unpadded base64url: %w", err)
	}
	return trust.NewRegistry(trust.Anchor{KeyID: keyID, PublicKey: publicKey})
}

func moduleRootDir() string {
	if dir := strings.TrimSpace(os.Getenv("PAYESH_MODULE_ROOT")); dir != "" {
		return dir
	}
	return "/var/lib/payesh/modules"
}

// unavailableCPUService keeps the documented route honest without linking
// the optional CPU implementation into the base server. The real service is
// supplied only by the future authenticated privileged module host.
type unavailableCPUService struct{}

type handlerService struct{ handler http.Handler }

type privdModuleExecutor struct{ client privd.Client }

func (e privdModuleExecutor) Invoke(ctx context.Context, invocation modules.ModuleInvocation) error {
	return e.client.Invoke(ctx, privd.ModuleInvocation{ServerID: invocation.ServerID, ModuleID: invocation.ModuleID, ModuleVersion: invocation.ModuleVersion, Operation: invocation.Operation, InstallDir: invocation.InstallDir})
}

func (s handlerService) Handler() http.Handler { return s.handler }

func (unavailableCPUService) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":"module_executor_unavailable","message":"CPU Controls requires the authenticated privileged module executor","retryable":false}` + "\n"))
	})
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
