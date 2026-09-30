package webtls

import (
	"bufio"
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"time"
)

// Listener serves plain HTTP and, once a certificate exists, HTTPS on the
// same port. The first byte of each connection selects the protocol: a TLS
// handshake always starts with 0x16. Sniffing happens off the accept loop so
// a slow client cannot stall other connections.
type Listener struct {
	net.Listener
	manager *Manager
	config  *tls.Config
	conns   chan net.Conn
	errs    chan error
	done    chan struct{}
	once    sync.Once
}

// NewListener wraps inner. It must be served with http.Server.Serve.
func NewListener(inner net.Listener, manager *Manager) *Listener {
	l := &Listener{
		Listener: inner,
		manager:  manager,
		config:   &tls.Config{MinVersion: tls.VersionTLS12, ClientAuth: tls.RequestClientCert, GetCertificate: manager.GetCertificate, NextProtos: []string{"http/1.1"}},
		conns:    make(chan net.Conn),
		errs:     make(chan error, 1),
		done:     make(chan struct{}),
	}
	go l.acceptLoop()
	return l
}

func (l *Listener) acceptLoop() {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			select {
			case l.errs <- err:
			case <-l.done:
			}
			return
		}
		go l.sniff(conn)
	}
}

func (l *Listener) sniff(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)
	first, err := reader.Peek(1)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		_ = conn.Close()
		return
	}
	var result net.Conn = &peekedConn{Conn: conn, reader: reader}
	if first[0] == 0x16 {
		if !l.manager.Active() {
			_ = conn.Close()
			return
		}
		result = tls.Server(result, l.config)
	}
	select {
	case l.conns <- result:
	case <-l.done:
		_ = conn.Close()
	}
}

func (l *Listener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case err := <-l.errs:
		return nil, err
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *Listener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

type peekedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// RedirectToHTTPS sends plain-HTTP browser requests to HTTPS once a
// certificate is active. /healthz stays reachable over HTTP for local checks,
// and so does reading the HTTPS status: a dashboard opened over HTTP polls it
// to learn that HTTPS is ready and then moves itself to the HTTPS address.
func (m *Manager) RedirectToHTTPS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		statusRead := r.Method == http.MethodGet && r.URL.Path == "/api/v1/settings/https"
		if r.TLS != nil || !m.Active() || r.URL.Path == "/healthz" || statusRead {
			next.ServeHTTP(w, r)
			return
		}
		host := m.Domain()
		if port := m.Port; port != "" && port != "443" {
			host = net.JoinHostPort(host, port)
		}
		// Temporary redirects: a browser must not keep forcing HTTPS if the
		// operator later removes the domain.
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	})
}
