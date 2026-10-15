package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	installpkg "github.com/Real-kia/payesh/internal/install"
	"github.com/Real-kia/payesh/internal/netproxy"
)

// runSSHProxyBridge is OpenSSH's ProxyCommand for proxied installations:
// `payesh-server ssh-proxy-bridge HOST PORT`. The proxy URL comes from the
// environment so its password never appears in a process listing.
func runSSHProxyBridge(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: payesh-server ssh-proxy-bridge HOST PORT")
		return 2
	}
	proxy, err := netproxy.Parse(os.Getenv(installpkg.SSHProxyEnv))
	if err != nil || proxy == nil {
		fmt.Fprintln(os.Stderr, "ssh-proxy-bridge: "+installpkg.SSHProxyEnv+" must hold an http:// or socks5:// proxy URL")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if err := netproxy.Bridge(ctx, proxy, net.JoinHostPort(args[0], args[1]), os.Stdin, os.Stdout); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "ssh-proxy-bridge:", err)
		return 1
	}
	return 0
}
