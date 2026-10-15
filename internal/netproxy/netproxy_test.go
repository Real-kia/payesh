package netproxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// echoServer accepts connections and echoes each one back.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	return ln.Addr().String()
}

func startRelay(t *testing.T, grants []Grant) (*Relay, string) {
	t.Helper()
	relay := NewRelay()
	relay.SetGrants(grants)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = relay.Serve(ln) }()
	return relay, ln.Addr().String()
}

func roundTrip(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != "ping" {
		t.Fatalf("echo = %q, %v", got, err)
	}
}

func TestParse(t *testing.T) {
	for raw, want := range map[string]string{
		"http://user:p%40ss@10.0.0.1:3128": "http://10.0.0.1:3128",
		"socks5://127.0.0.1:1080":          "socks5://127.0.0.1:1080",
		"SOCKS5H://proxy.example:1080":     "socks5://proxy.example:1080",
	} {
		p, err := Parse(raw)
		if err != nil || p.Redacted() != want {
			t.Fatalf("Parse(%q) = %v, %v", raw, p.Redacted(), err)
		}
	}
	p, _ := Parse("http://user:p%40ss@10.0.0.1:3128")
	if p.Username != "user" || p.Password != "p@ss" {
		t.Fatalf("credentials = %q/%q", p.Username, p.Password)
	}
	if p, err := Parse(""); p != nil || err != nil {
		t.Fatal("empty URL must mean no proxy")
	}
	for _, bad := range []string{"https://h:1", "ftp://h:1", "http://h", "http://h:0", "http://h:1/path", "http://h:1?x=1"} {
		if _, err := Parse(bad); err == nil {
			t.Fatalf("Parse(%q) accepted", bad)
		}
	}
}

func TestRelayTunnelsOnlyGrantedDestinations(t *testing.T) {
	target := echoServer(t)
	other := echoServer(t)
	_, relayAddr := startRelay(t, []Grant{{Username: "install-1", PasswordSHA256: HashPassword("secret"), Destinations: []string{target}}})
	ctx := context.Background()

	ok, _ := Parse("http://install-1:secret@" + relayAddr)
	conn, err := ok.DialContext(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, conn)
	conn.Close()

	if _, err := ok.DialContext(ctx, other); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("ungranted destination: %v", err)
	}
	wrong, _ := Parse("http://install-1:nope@" + relayAddr)
	if _, err := wrong.DialContext(ctx, target); err == nil || !strings.Contains(err.Error(), "407") {
		t.Fatalf("wrong password: %v", err)
	}
	unknown, _ := Parse("http://ghost:secret@" + relayAddr)
	if _, err := unknown.DialContext(ctx, target); err == nil || !strings.Contains(err.Error(), "407") {
		t.Fatalf("unknown user: %v", err)
	}
	anonymous, _ := Parse("http://" + relayAddr)
	if _, err := anonymous.DialContext(ctx, target); err == nil || !strings.Contains(err.Error(), "407") {
		t.Fatalf("no credentials: %v", err)
	}
}

func TestRelayGrantExpiryAndReplacement(t *testing.T) {
	target := echoServer(t)
	clock := time.Unix(1000, 0)
	relay, relayAddr := startRelay(t, nil)
	relay.now = func() time.Time { return clock }
	relay.SetGrants([]Grant{{Username: "u", PasswordSHA256: HashPassword("p"), Destinations: []string{target}, ExpiresAt: clock.Add(time.Minute)}})
	p, _ := Parse("http://u:p@" + relayAddr)
	conn, err := p.DialContext(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	clock = clock.Add(time.Minute)
	if _, err := p.DialContext(context.Background(), target); err == nil {
		t.Fatal("expired grant still works")
	}
	relay.SetGrants(nil)
	clock = time.Unix(1000, 0)
	if _, err := p.DialContext(context.Background(), target); err == nil {
		t.Fatal("removed grant still works")
	}
}

func TestRelayListenerApply(t *testing.T) {
	target := echoServer(t)
	probe, _ := net.Listen("tcp", "127.0.0.1:0")
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()
	var listener RelayListener
	grant := Grant{Username: "n", PasswordSHA256: HashPassword("p"), Destinations: []string{target}}
	if err := listener.Apply(port, []Grant{grant}); err != nil {
		t.Fatal(err)
	}
	p, _ := Parse("http://n:p@127.0.0.1:" + strconv.Itoa(port))
	conn, err := p.DialContext(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, conn)
	conn.Close()
	if err := listener.Apply(0, nil); err != nil || listener.Port() != 0 {
		t.Fatalf("stop: %v port=%d", err, listener.Port())
	}
	if _, err := p.DialContext(context.Background(), target); err == nil {
		t.Fatal("stopped relay still accepts")
	}
}

// socksServer is a minimal SOCKS5 server for client tests. When user is set it
// requires username/password authentication.
func socksServer(t *testing.T, user, pass string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				head := make([]byte, 2)
				if _, err := io.ReadFull(conn, head); err != nil {
					return
				}
				methods := make([]byte, head[1])
				_, _ = io.ReadFull(conn, methods)
				want := byte(0x00)
				if user != "" {
					want = 0x02
				}
				if !bytes.Contains(methods, []byte{want}) {
					_, _ = conn.Write([]byte{5, 0xff})
					return
				}
				_, _ = conn.Write([]byte{5, want})
				if want == 0x02 {
					version := make([]byte, 2)
					_, _ = io.ReadFull(conn, version)
					u := make([]byte, version[1])
					_, _ = io.ReadFull(conn, u)
					l := make([]byte, 1)
					_, _ = io.ReadFull(conn, l)
					p := make([]byte, l[0])
					_, _ = io.ReadFull(conn, p)
					if string(u) != user || string(p) != pass {
						_, _ = conn.Write([]byte{1, 1})
						return
					}
					_, _ = conn.Write([]byte{1, 0})
				}
				req := make([]byte, 4)
				if _, err := io.ReadFull(conn, req); err != nil {
					return
				}
				var host string
				switch req[3] {
				case 0x01:
					ip := make([]byte, 4)
					_, _ = io.ReadFull(conn, ip)
					host = net.IP(ip).String()
				case 0x03:
					l := make([]byte, 1)
					_, _ = io.ReadFull(conn, l)
					name := make([]byte, l[0])
					_, _ = io.ReadFull(conn, name)
					host = string(name)
				}
				portBytes := make([]byte, 2)
				_, _ = io.ReadFull(conn, portBytes)
				upstream, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(portBytes)))))
				if err != nil {
					_, _ = conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				defer upstream.Close()
				_, _ = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
				go func() { _, _ = io.Copy(upstream, conn) }()
				_, _ = io.Copy(conn, upstream)
			}(conn)
		}
	}()
	return ln.Addr().String()
}

func TestSOCKS5Client(t *testing.T) {
	target := echoServer(t)
	_, port, _ := net.SplitHostPort(target)
	ctx := context.Background()

	open, _ := Parse("socks5://" + socksServer(t, "", ""))
	conn, err := open.DialContext(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, conn)
	conn.Close()

	authAddr := socksServer(t, "alice", "pw")
	authed, _ := Parse("socks5://alice:pw@" + authAddr)
	conn, err = authed.DialContext(ctx, "localhost:"+port)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, conn)
	conn.Close()

	bad, _ := Parse("socks5://alice:wrong@" + authAddr)
	if _, err := bad.DialContext(ctx, target); err == nil {
		t.Fatal("wrong SOCKS5 password accepted")
	}
	if _, err := authed.DialContext(ctx, "127.0.0.1:1"); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("unreachable destination: %v", err)
	}
}

func TestBridgeCarriesBytes(t *testing.T) {
	target := echoServer(t)
	_, relayAddr := startRelay(t, []Grant{{Username: "u", PasswordSHA256: HashPassword("p"), Destinations: []string{target}}})
	p, _ := Parse("http://u:p@" + relayAddr)
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	errs := make(chan error, 1)
	go func() { errs <- Bridge(context.Background(), p, target, stdinReader, stdoutWriter) }()
	go func() { _, _ = stdinWriter.Write([]byte("hello")) }()
	got := make([]byte, 5)
	if _, err := io.ReadFull(stdoutReader, got); err != nil || string(got) != "hello" {
		t.Fatalf("bridge echo = %q, %v", got, err)
	}
	stdinWriter.Close()
	select {
	case <-errs:
	case <-time.After(5 * time.Second):
		t.Fatal("bridge did not finish after stdin closed")
	}
}
