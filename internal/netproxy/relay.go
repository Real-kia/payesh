package netproxy

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	relayMaxConnections = 256
	relayHeaderTimeout  = 15 * time.Second
	relayDialTimeout    = 15 * time.Second
)

// Grant lets one credential open tunnels to an exact set of host:port
// destinations until ExpiresAt (zero means no expiry). Only the SHA-256 of the
// password is held, so the relay never stores a usable secret.
type Grant struct {
	Username       string
	PasswordSHA256 string
	Destinations   []string
	ExpiresAt      time.Time
}

// HashPassword returns the hex SHA-256 used in Grant.PasswordSHA256.
func HashPassword(password string) string {
	sum := sha256.Sum256([]byte(password))
	return hex.EncodeToString(sum[:])
}

// Relay is an authenticated HTTP CONNECT proxy. It is not an open proxy:
// every tunnel needs a valid grant naming the exact destination.
type Relay struct {
	mu     sync.RWMutex
	grants map[string]Grant
	slots  chan struct{}
	now    func() time.Time
	dial   func(ctx context.Context, addr string) (net.Conn, error)
}

func NewRelay() *Relay {
	return &Relay{grants: map[string]Grant{}, slots: make(chan struct{}, relayMaxConnections)}
}

// SetGrants replaces the full grant set. Open tunnels are not interrupted.
func (r *Relay) SetGrants(grants []Grant) {
	next := make(map[string]Grant, len(grants))
	for _, grant := range grants {
		if grant.Username == "" || len(grant.PasswordSHA256) != 64 {
			continue
		}
		next[grant.Username] = grant
	}
	r.mu.Lock()
	r.grants = next
	r.mu.Unlock()
}

func (r *Relay) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// Serve accepts connections until the listener is closed.
func (r *Relay) Serve(listener net.Listener) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return err
		}
		select {
		case r.slots <- struct{}{}:
			go func() {
				defer func() { <-r.slots }()
				r.handle(conn)
			}()
		default:
			_, _ = io.WriteString(conn, "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\n\r\n")
			_ = conn.Close()
		}
	}
}

func (r *Relay) authorize(request *http.Request) (int, string) {
	header := request.Header.Get("Proxy-Authorization")
	scheme, encoded, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Basic") {
		return http.StatusProxyAuthRequired, ""
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return http.StatusProxyAuthRequired, ""
	}
	username, password, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return http.StatusProxyAuthRequired, ""
	}
	r.mu.RLock()
	grant, found := r.grants[username]
	r.mu.RUnlock()
	presented := HashPassword(password)
	expected := grant.PasswordSHA256
	if !found {
		expected = strings.Repeat("0", 64)
	}
	if subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) != 1 || !found {
		return http.StatusProxyAuthRequired, ""
	}
	if !grant.ExpiresAt.IsZero() && !r.clock().Before(grant.ExpiresAt) {
		return http.StatusProxyAuthRequired, ""
	}
	target := strings.ToLower(request.Host)
	for _, destination := range grant.Destinations {
		if strings.ToLower(destination) == target {
			return http.StatusOK, target
		}
	}
	return http.StatusForbidden, ""
}

func (r *Relay) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(relayHeaderTimeout))
	reader := bufio.NewReaderSize(conn, 4096)
	request, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	if request.Method != http.MethodConnect {
		_, _ = io.WriteString(conn, "HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 0\r\n\r\n")
		return
	}
	status, target := r.authorize(request)
	switch status {
	case http.StatusOK:
	case http.StatusProxyAuthRequired:
		_, _ = io.WriteString(conn, "HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"payesh-relay\"\r\nContent-Length: 0\r\n\r\n")
		return
	default:
		_, _ = io.WriteString(conn, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
		return
	}
	dial := r.dial
	if dial == nil {
		dial = func(ctx context.Context, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), relayDialTimeout)
	upstream, err := dial(ctx, target)
	cancel()
	if err != nil {
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	defer upstream.Close()
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetKeepAlive(true)
		_ = tcp.SetKeepAlivePeriod(30 * time.Second)
	}
	done := make(chan struct{}, 2)
	go func() {
		// Bytes the client sent right after the request are already buffered.
		_, _ = io.Copy(upstream, reader)
		if tcp, ok := upstream.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, upstream)
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	// One side finished; give the other a bounded time to drain.
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	_ = upstream.SetDeadline(time.Now().Add(30 * time.Second))
	<-done
}

// RelayListener runs a Relay on a TCP port and can be reconfigured or
// stopped at runtime. It is what a node's agent uses.
type RelayListener struct {
	mu       sync.Mutex
	relay    *Relay
	listener net.Listener
	port     int
}

// Apply starts, restarts (port change), updates or stops the relay.
// port <= 0 stops it.
func (l *RelayListener) Apply(port int, grants []Grant) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.relay == nil {
		l.relay = NewRelay()
	}
	if port <= 0 {
		l.stopLocked()
		return nil
	}
	l.relay.SetGrants(grants)
	if l.listener != nil && l.port == port {
		return nil
	}
	l.stopLocked()
	listener, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(port)))
	if err != nil {
		return err
	}
	l.listener, l.port = listener, port
	relay := l.relay
	go func() { _ = relay.Serve(listener) }()
	return nil
}

// Port reports the active port, or 0 when stopped.
func (l *RelayListener) Port() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.listener == nil {
		return 0
	}
	return l.port
}

func (l *RelayListener) stopLocked() {
	if l.listener != nil {
		_ = l.listener.Close()
		l.listener, l.port = nil, 0
	}
}
