package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/cutoverpeer"
	"github.com/Real-kia/payesh/internal/monitoring"
	"github.com/Real-kia/payesh/internal/updater"
)

// cutoverServeReady, when set, is told the bound address and certificate pin once
// the peer is listening. Tests use it; the same facts are printed as JSON.
var cutoverServeReady func(addr, pin string)

const cutoverUsage = `usage: payesh cutover <identity|grant|serve|run|status> [options]

  identity   create a client certificate and key for running a cutover (prints its fingerprint)
  grant      let one client fingerprint run one cutover for one server on this destination
  serve      serve the destination peer over mutual TLS (run on the destination)
  run        run a cutover from this source to a destination peer
  status     print a cutover's journal record`

func printCutoverJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

// cutoverCommand runs a cutover subcommand, writing results to standard output.
func cutoverCommand(ctx context.Context, args []string) error {
	return cutoverCommandTo(ctx, os.Stdout, args)
}

// cutoverCommandTo is cutoverCommand with an explicit output writer.
func cutoverCommandTo(ctx context.Context, out io.Writer, args []string) error {
	if len(args) == 0 {
		return errors.New(cutoverUsage)
	}
	switch args[0] {
	case "identity":
		return cutoverIdentity(out, args[1:])
	case "grant":
		return cutoverGrant(out, args[1:])
	case "serve":
		return cutoverServe(ctx, out, args[1:])
	case "run":
		return cutoverRun(ctx, out, args[1:])
	case "status":
		return cutoverStatus(ctx, out, args[1:])
	default:
		return fmt.Errorf("unknown cutover subcommand %q\n%s", args[0], cutoverUsage)
	}
}

func cutoverIdentity(out io.Writer, args []string) error {
	flags := flag.NewFlagSet("cutover identity", flag.ContinueOnError)
	dir := flags.String("dir", "", "directory to create the certificate and key in")
	name := flags.String("name", "payesh-cutover-client", "certificate common name")
	days := flags.Int("days", 30, "certificate validity in days")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dir == "" || *days < 1 || *days > 365 {
		return errors.New("usage: payesh cutover identity --dir DIR [--name NAME] [--days 1..365]")
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return err
	}
	certPath, keyPath := filepath.Join(*dir, "client-cert.pem"), filepath.Join(*dir, "client-key.pem")
	fingerprint, err := cutoverpeer.WriteCredentials(certPath, keyPath, *name, time.Now().AddDate(0, 0, *days))
	if err != nil {
		return err
	}
	return printCutoverJSON(out, map[string]string{"fingerprint": fingerprint, "certificate_file": certPath, "key_file": keyPath})
}

func cutoverGrant(out io.Writer, args []string) error {
	flags := flag.NewFlagSet("cutover grant", flag.ContinueOnError)
	grants := flags.String("grants-file", "", "grant file the serving peer reads")
	serverID := flags.String("server-id", "", "the one server this grant covers")
	cutoverID := flags.String("cutover-id", "", "the one cutover this grant covers")
	fingerprint := flags.String("client-fingerprint", "", "SHA-256 fingerprint printed by `payesh cutover identity`")
	ttl := flags.Duration("ttl", time.Hour, "how long the grant lasts (at most 24h)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *grants == "" || *serverID == "" || *cutoverID == "" || *fingerprint == "" {
		return errors.New("usage: payesh cutover grant --grants-file F --server-id ID --cutover-id ID --client-fingerprint HEX [--ttl 1h]")
	}
	now := time.Now()
	grant := cutoverpeer.Grant{ClientFingerprint: *fingerprint, ServerID: contracts.ServerID(*serverID), CutoverID: *cutoverID, ExpiresAt: now.Add(*ttl)}
	if err := cutoverpeer.AddGrant(*grants, grant, now); err != nil {
		return err
	}
	return printCutoverJSON(out, map[string]any{"server_id": *serverID, "cutover_id": *cutoverID, "expires_at": grant.ExpiresAt.UTC()})
}

// openCLIDatabase opens a SQLite file that other processes may read while a
// cutover writes it, such as `payesh cutover status`. Without a busy timeout the
// writer fails at once with SQLITE_BUSY and the run exits mid-phase.
func openCLIDatabase(path string) (*sql.DB, error) {
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=busy_timeout(10000)"}).String()
	return sql.Open("sqlite", dsn)
}

func openStoreAndDB(ctx context.Context, path string) (*monitoring.Store, *sql.DB, error) {
	store, err := monitoring.OpenStore(ctx, path, localStoreOptions())
	if err != nil {
		return nil, nil, err
	}
	db, err := openCLIDatabase(path)
	if err != nil {
		_ = store.Close()
		return nil, nil, err
	}
	return store, db, nil
}

func cutoverServe(ctx context.Context, out io.Writer, args []string) error {
	flags := flag.NewFlagSet("cutover serve", flag.ContinueOnError)
	dbPath := flags.String("db", "", "this destination's SQLite database")
	listen := flags.String("listen", "127.0.0.1:9798", "address to listen on (mutual TLS is always required)")
	certPath := flags.String("tls-cert", "", "peer certificate PEM")
	keyPath := flags.String("tls-key", "", "peer private key PEM (mode 0600)")
	generate := flags.Bool("generate-tls", false, "create the certificate and key if they do not exist")
	grants := flags.String("grants-file", "", "grant file (re-read on every request)")
	spool := flags.String("spool-dir", "", "private directory for in-flight artifacts")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dbPath == "" || *certPath == "" || *keyPath == "" || *grants == "" || *spool == "" {
		return errors.New("usage: payesh cutover serve --db PATH --tls-cert F --tls-key F --grants-file F --spool-dir D [--listen ADDR] [--generate-tls]")
	}
	if *generate {
		_, certErr := os.Stat(*certPath)
		_, keyErr := os.Stat(*keyPath)
		if errors.Is(certErr, os.ErrNotExist) && errors.Is(keyErr, os.ErrNotExist) {
			if _, err := cutoverpeer.WriteCredentials(*certPath, *keyPath, "payesh-cutover-peer", time.Now().AddDate(0, 0, 90)); err != nil {
				return err
			}
		}
	}
	certificate, pin, err := cutoverpeer.LoadCredentials(*certPath, *keyPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*spool, 0o700); err != nil {
		return err
	}
	store, db, err := openStoreAndDB(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	defer db.Close()
	peer := &cutoverpeer.Server{DB: db, Store: store, SpoolDir: *spool, GrantsFunc: func() ([]cutoverpeer.Grant, error) { return cutoverpeer.LoadGrants(*grants) }}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: peer.Handler(), TLSConfig: cutoverpeer.ServerTLSConfig(certificate), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 5 * time.Minute, WriteTimeout: 5 * time.Minute, IdleTimeout: 2 * time.Minute}
	stop, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	failed := make(chan error, 1)
	go func() { failed <- server.Serve(tls.NewListener(listener, server.TLSConfig)) }()
	addr := listener.Addr().String()
	if cutoverServeReady != nil {
		cutoverServeReady(addr, pin)
	}
	// One compact line, so a supervisor can read the address and pin without a JSON stream parser.
	if err := json.NewEncoder(out).Encode(map[string]string{"listening": addr, "pin": pin}); err != nil {
		return err
	}
	select {
	case err := <-failed:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-stop.Done():
		shutdown, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer done()
		if err := server.Shutdown(shutdown); err != nil {
			return err
		}
		return nil
	}
}

func cutoverRun(ctx context.Context, out io.Writer, args []string) error {
	flags := flag.NewFlagSet("cutover run", flag.ContinueOnError)
	dbPath := flags.String("db", "", "this source's SQLite database")
	serverID := flags.String("server-id", "", "the server whose authority moves")
	cutoverID := flags.String("cutover-id", "", "a new identifier for this cutover (never reused after a failure)")
	sourceID := flags.String("source-id", "", "authority owner identifier for this source")
	destinationID := flags.String("destination-id", "", "authority owner identifier for the destination")
	sourceRole := flags.String("source-role", "", "source role (standalone, hub, node, cli-only)")
	destinationRole := flags.String("destination-role", "", "destination role (standalone, hub, node, cli-only)")
	managed := flags.Int("managed-nodes", 0, "nodes this hub still manages")
	disposition := flags.String("fleet-disposition", "", "migrated or detached, when a hub still manages nodes")
	peerURL := flags.String("peer-url", "", "https://host:port of the destination peer")
	peerPin := flags.String("peer-pin", "", "SHA-256 pin of the destination certificate printed by `serve`")
	clientCert := flags.String("client-cert", "", "client certificate PEM from `cutover identity`")
	clientKey := flags.String("client-key", "", "client private key PEM (mode 0600)")
	stateDir := flags.String("state-dir", "", "private directory for the journal, backup and artifacts")
	if err := flags.Parse(args); err != nil {
		return err
	}
	for name, value := range map[string]string{"--db": *dbPath, "--server-id": *serverID, "--cutover-id": *cutoverID, "--source-id": *sourceID, "--destination-id": *destinationID,
		"--source-role": *sourceRole, "--destination-role": *destinationRole, "--peer-url": *peerURL, "--peer-pin": *peerPin, "--client-cert": *clientCert, "--client-key": *clientKey, "--state-dir": *stateDir} {
		if value == "" {
			return fmt.Errorf("cutover run requires %s", name)
		}
	}
	certificate, _, err := cutoverpeer.LoadCredentials(*clientCert, *clientKey)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*stateDir, 0o700); err != nil {
		return err
	}
	store, db, err := openStoreAndDB(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	defer db.Close()
	journal, err := openCLIDatabase(filepath.Join(*stateDir, "cutover-journal.db"))
	if err != nil {
		return err
	}
	defer journal.Close()
	cutover := &updater.StoreCutover{Source: store, SourceDB: db, ServerID: contracts.ServerID(*serverID), StateDir: filepath.Join(*stateDir, "artifacts"),
		Peer: &cutoverpeer.Client{BaseURL: *peerURL, ServerCertSHA256: *peerPin, ClientCert: certificate, ServerID: contracts.ServerID(*serverID)}}
	request := updater.RoleCutoverRequest{ID: *cutoverID, SourceRole: *sourceRole, DestinationRole: *destinationRole, SourceID: *sourceID, DestinationID: *destinationID, ManagedNodes: *managed, FleetDisposition: *disposition}
	record, runErr := updater.RunRoleCutover(ctx, journal, request, cutover.Hooks(), time.Now)
	if printErr := printCutoverJSON(out, record); printErr != nil && runErr == nil {
		return printErr
	}
	return runErr
}

func cutoverStatus(ctx context.Context, out io.Writer, args []string) error {
	flags := flag.NewFlagSet("cutover status", flag.ContinueOnError)
	journalPath := flags.String("journal", "", "cutover journal database")
	cutoverID := flags.String("cutover-id", "", "cutover identifier")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *journalPath == "" || *cutoverID == "" {
		return errors.New("usage: payesh cutover status --journal PATH --cutover-id ID")
	}
	if _, err := os.Stat(*journalPath); err != nil {
		return err
	}
	journal, err := openCLIDatabase(*journalPath)
	if err != nil {
		return err
	}
	defer journal.Close()
	record, found, err := updater.LoadRoleCutover(ctx, journal, *cutoverID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no cutover %q in this journal", *cutoverID)
	}
	return printCutoverJSON(out, record)
}
