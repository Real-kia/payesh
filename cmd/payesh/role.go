package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"

	"github.com/Real-kia/payesh/internal/install"
	"github.com/Real-kia/payesh/internal/release"
)

func roleCommand(ctx context.Context, args []string) error {
	if len(args) < 2 || args[0] != "convert" || args[1] != "node" {
		return errors.New("usage: payesh role convert node --transport-url wss://HUB --node-identity-file PATH --hub-ca-file PATH [--check]")
	}
	flags := flag.NewFlagSet("role convert node", flag.ContinueOnError)
	url := flags.String("transport-url", "", "destination hub transport URL")
	identity := flags.String("node-identity-file", "", "enrolled node identity JSON")
	ca := flags.String("hub-ca-file", "", "destination hub CA PEM")
	check := flags.Bool("check", false, "check conversion without applying")
	if err := flags.Parse(args[2:]); err != nil {
		return err
	}
	if len(flags.Args()) != 0 {
		return errors.New("unexpected role conversion arguments")
	}
	if runtime.GOOS != "linux" {
		return errors.New("role conversion requires Linux")
	}
	if os.Geteuid() != 0 {
		return errors.New("run role conversion as root")
	}
	source, err := installedRole("/var/lib/payesh/install-state.json")
	if err != nil {
		return err
	}
	cfg := install.ConversionConfig{FromRole: source, TransportURL: *url, NodeIdentityFile: *identity, HubCAFile: *ca}
	if err := install.CheckHubToNode(ctx, "/", cfg); err != nil {
		return err
	}
	if err := install.CheckConversionTransport(ctx, cfg); err != nil {
		return fmt.Errorf("destination node transport: %w", err)
	}
	if *check {
		fmt.Println("Hub-to-node conversion is ready.")
		return nil
	}
	token := os.Getenv("GITHUB_TOKEN")
	latest, err := (release.GitHubClient{Token: token}).Latest(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Converting %s to an enrolled node with Payesh %s...\n", source, latest.Version)
	return runReleaseInstaller(ctx, latest.Version, token, "--role", "node", "--version", latest.Version, "--convert-from", source, "--transport-url", *url, "--node-identity-file", *identity, "--hub-ca-file", *ca)
}
