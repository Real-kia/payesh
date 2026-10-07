package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Real-kia/payesh/internal/collector"
	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/privd"
	"github.com/Real-kia/payesh/internal/transport"
	"github.com/Real-kia/payesh/internal/version"
	"github.com/Real-kia/payesh/internal/webupdate"
)

func main() {
	root := flag.String("root", "/", "filesystem root containing proc-like host files")
	serverID := flag.String("server-id", "", "stable server id (16..128 URL-safe characters); omitted values use the persisted installation identity")
	identityFile := flag.String("identity-file", "/var/lib/payesh/server-id", "path for the persisted installation server id")
	epoch := flag.String("epoch", "", "collector epoch override; omitted values get a fresh epoch per start")
	billingInterfaces := flag.String("billing-interfaces", "", "comma-separated authoritative billing interfaces; empty uses safe discovery")
	interval := flag.Duration("interval", 0, "repeat collection interval; zero emits one sample")
	settingsDB := flag.String("settings-db", "/var/lib/payesh/payesh.db", "read-only local storage sampling settings")
	var hubSampleSeconds atomic.Int64
	transportURL := flag.String("transport-url", os.Getenv("PAYESH_TRANSPORT_URL"), "optional wss:// node transport endpoint; empty keeps stdout mode")
	bootstrapURL := flag.String("bootstrap-url", os.Getenv("PAYESH_BOOTSTRAP_URL"), "optional wss:// protected bootstrap endpoint; used only when node identity is absent")
	bootstrapJob := flag.String("bootstrap-job", os.Getenv("PAYESH_BOOTSTRAP_JOB"), "durable enrollment job id for protected bootstrap")
	bootstrapToken := flag.String("bootstrap-token", os.Getenv("PAYESH_BOOTSTRAP_TOKEN"), "single-use enrollment token for protected bootstrap")
	nodeIdentityFile := flag.String("node-identity-file", envOrDefault("PAYESH_NODE_IDENTITY_FILE", "/var/lib/payesh/node-identity.json"), "persisted enrolled node certificate/key JSON")
	hubTrustFile := flag.String("hub-trust-file", envOrDefault("PAYESH_HUB_TRUST_FILE", "/var/lib/payesh/hub-ca.pem"), "hub TLS trust-anchor PEM")
	spoolFile := flag.String("spool-file", envOrDefault("PAYESH_SPOOL_FILE", "/var/lib/payesh/agent.spool"), "bounded offline sample spool")
	privdSocket := flag.String("privd-socket", os.Getenv("PAYESH_PRIVD_SOCKET"), "optional privileged-helper socket enabling authenticated action execution")
	checkTransport := flag.Bool("check-transport", false, "verify the configured hub TLS node port and exit without collecting metrics")
	printVersion := flag.Bool("version", false, "print the agent release version and exit")
	flag.Parse()
	if *printVersion {
		fmt.Println(version.Value)
		return
	}

	collectorEpoch := contracts.CollectorEpoch(*epoch)
	if collectorEpoch == "" {
		collectorEpoch = collector.NewEpoch()
	}
	identity := contracts.ServerID(*serverID)
	var configuredTransport agentTransportConfig
	var configuredNodeIdentity transport.NodeIdentity
	configuredTransportURL := strings.TrimSpace(*transportURL)
	if strings.TrimSpace(*bootstrapURL) != "" && configuredTransportURL == "" {
		var err error
		configuredTransportURL, err = nodeTransportURLFromBootstrap(*bootstrapURL)
		if err != nil {
			fmt.Fprintln(os.Stderr, "configure bootstrap transport:", err)
			os.Exit(2)
		}
	}
	if configuredTransportURL != "" {
		var err error
		configuredTransport, err = newAgentTransportConfig(configuredTransportURL, *nodeIdentityFile, *hubTrustFile, *spoolFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "configure node transport:", err)
			os.Exit(2)
		}
		configuredNodeIdentity, err = transport.LoadNodeIdentity(configuredTransport.IdentityFile)
		if errors.Is(err, os.ErrNotExist) && strings.TrimSpace(*bootstrapURL) != "" && !*checkTransport {
			if strings.TrimSpace(*bootstrapJob) == "" || strings.TrimSpace(*bootstrapToken) == "" {
				fmt.Fprintln(os.Stderr, "bootstrap requires --bootstrap-job and --bootstrap-token when node identity is absent")
				os.Exit(2)
			}
			trustPEM, readErr := os.ReadFile(configuredTransport.TrustFile)
			if readErr != nil {
				fmt.Fprintln(os.Stderr, "read hub trust anchor:", readErr)
				os.Exit(1)
			}
			bootstrapCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			configuredNodeIdentity, err = transport.BootstrapAgent(bootstrapCtx, strings.TrimSpace(*bootstrapURL), strings.TrimSpace(*bootstrapJob), strings.TrimSpace(*bootstrapToken), trustPEM, nil)
			cancel()
			if err != nil {
				fmt.Fprintln(os.Stderr, "bootstrap node identity:", err)
				os.Exit(1)
			}
			if err := transport.SaveNodeIdentity(configuredTransport.IdentityFile, configuredNodeIdentity); err != nil {
				fmt.Fprintln(os.Stderr, "persist node identity:", err)
				os.Exit(1)
			}
		} else if err != nil {
			fmt.Fprintln(os.Stderr, "load node identity:", err)
			os.Exit(1)
		}
		if identity != "" && identity != configuredNodeIdentity.ServerID {
			fmt.Fprintln(os.Stderr, "server identity does not match enrolled node certificate")
			os.Exit(2)
		}
		// In transport mode the enrolled certificate is the authoritative
		// identity. This prevents a stale local server-id file from producing
		// samples that the authenticated hub must reject.
		identity = configuredNodeIdentity.ServerID
	}
	if *checkTransport {
		if configuredTransportURL == "" {
			fmt.Fprintln(os.Stderr, "transport check requires a hub transport URL")
			os.Exit(2)
		}
		trustPEM, err := os.ReadFile(configuredTransport.TrustFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "read hub trust anchor:", err)
			os.Exit(1)
		}
		checker := &transport.AgentClient{URL: configuredTransport.URL, IdentityPath: configuredTransport.IdentityFile, Identity: configuredNodeIdentity, TrustPEM: trustPEM}
		if err = checker.CheckEndpoint(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, "hub node transport check failed; check the hub listener, firewall, DNS and TLS trust")
			os.Exit(1)
		}
		fmt.Println("Hub node transport port is reachable and TLS is trusted.")
		return
	}
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
	var transportBatches chan contracts.SampleBatch
	var transportDone <-chan error
	if configuredTransportURL != "" {
		trustPEM, err := os.ReadFile(configuredTransport.TrustFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "read hub trust anchor:", err)
			os.Exit(1)
		}
		spool, err := transport.OpenSpool(configuredTransport.SpoolFile, transport.MaxSpoolBytes)
		if err != nil {
			fmt.Fprintln(os.Stderr, "open sample spool:", err)
			os.Exit(1)
		}
		capabilities := []string{"metrics", "traffic", "sampling-policy", transport.MigrationCapability}
		var actionHandler func(context.Context, contracts.ActionRequest) contracts.ActionResponse
		if socket := strings.TrimSpace(*privdSocket); socket != "" {
			capabilities = append(capabilities, "actions")
			helper := privd.Client{SocketPath: socket}
			actionHandler = helper.Execute
		}
		if webupdate.Supported(*root) {
			capabilities = append(capabilities, "actions", "core-update")
			actionHandler = webupdate.CoreActionHandler(filepath.Join(*root, "var/lib/payesh"), configuredNodeIdentity.ServerID, version.Value, actionHandler)
		}
		client := &transport.AgentClient{
			URL: configuredTransport.URL, Identity: configuredNodeIdentity, IdentityPath: configuredTransport.IdentityFile, TrustPEM: trustPEM, Spool: spool,
			Hello:          contracts.Hello{Version: version.Value, ProtocolMin: contracts.ProtocolVersion, ProtocolMax: contracts.ProtocolVersion, Architecture: runtime.GOARCH, Platform: runtime.GOOS, Capabilities: capabilities},
			ActionHandler:  actionHandler,
			SamplingPolicy: func(seconds int) { hubSampleSeconds.Store(int64(seconds)) },
		}
		transportBatches = make(chan contracts.SampleBatch, 1)
		done := make(chan error, 1)
		transportDone = done
		go func() { done <- client.Run(ctx, transportBatches) }()
	}
	var encoder *json.Encoder
	if transportBatches == nil {
		encoder = json.NewEncoder(os.Stdout)
		encoder.SetEscapeHTML(false)
	}
	for {
		sample, err := metricCollector.Collect(ctx, time.Now().UTC())
		if err != nil {
			fmt.Fprintln(os.Stderr, "collect:", err)
			os.Exit(1)
		}
		if transportBatches != nil {
			select {
			case transportBatches <- contracts.SampleBatch{Samples: []contracts.NodeMetricSample{sample}}:
			case err := <-transportDone:
				if err != nil && ctx.Err() == nil {
					fmt.Fprintln(os.Stderr, "node transport:", err)
					os.Exit(1)
				}
				return
			case <-ctx.Done():
				return
			}
		} else if err := encoder.Encode(sample); err != nil {
			fmt.Fprintln(os.Stderr, "encode:", err)
			os.Exit(1)
		}
		if *interval <= 0 {
			if transportBatches != nil {
				close(transportBatches)
				if err := <-transportDone; err != nil && ctx.Err() == nil {
					fmt.Fprintln(os.Stderr, "node transport:", err)
					os.Exit(1)
				}
			}
			return
		}
		effectiveInterval := *interval
		if transportBatches != nil {
			if seconds := hubSampleSeconds.Load(); seconds > 0 {
				effectiveInterval = time.Duration(seconds) * time.Second
			}
		} else {
			policyCtx, cancel := context.WithTimeout(ctx, time.Second)
			seconds := monitoring.ReadLocalSamplingSeconds(policyCtx, *settingsDB)
			cancel()
			if seconds > 0 {
				effectiveInterval = time.Duration(seconds) * time.Second
			}
		}
		timer := time.NewTimer(effectiveInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func nodeTransportURLFromBootstrap(endpoint string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u.Scheme != "wss" || u.Host == "" {
		return "", fmt.Errorf("bootstrap URL must be a wss:// endpoint")
	}
	u.Path = "/node/v1"
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

type agentTransportConfig struct {
	URL          string
	IdentityFile string
	TrustFile    string
	SpoolFile    string
}

func newAgentTransportConfig(endpoint, identityFile, trustFile, spoolFile string) (agentTransportConfig, error) {
	config := agentTransportConfig{URL: strings.TrimSpace(endpoint), IdentityFile: strings.TrimSpace(identityFile), TrustFile: strings.TrimSpace(trustFile), SpoolFile: strings.TrimSpace(spoolFile)}
	u, err := url.Parse(config.URL)
	if err != nil || u.Scheme != "wss" || u.Host == "" {
		return agentTransportConfig{}, fmt.Errorf("transport URL must be a wss:// endpoint")
	}
	for name, path := range map[string]string{"identity": config.IdentityFile, "trust": config.TrustFile, "spool": config.SpoolFile} {
		if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return agentTransportConfig{}, fmt.Errorf("%s path must be absolute and clean", name)
		}
	}
	return config, nil
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
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
