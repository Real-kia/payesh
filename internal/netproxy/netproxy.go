// Package netproxy dials TCP connections through an HTTP CONNECT or SOCKS5
// proxy and serves the small authenticated CONNECT relay that a node can run
// for servers the hub cannot reach directly. It has no dependencies beyond the
// standard library so the agent stays small.
package netproxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Proxy is a parsed proxy URL. The zero value is not usable; a nil *Proxy
// means "dial directly".
type Proxy struct {
	Scheme   string // "http" or "socks5"
	Host     string // host:port of the proxy
	Username string
	Password string
}

// Parse accepts http://[user:pass@]host:port, socks5://… and socks5h://….
// An empty string returns nil (no proxy).
func Parse(raw string) (*Proxy, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if len(raw) > 2048 {
		return nil, errors.New("proxy URL is too long")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("proxy URL is invalid")
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http":
	case "socks5", "socks5h":
		scheme = "socks5"
	default:
		return nil, errors.New("proxy URL must start with http://, socks5:// or socks5h://")
	}
	if u.Hostname() == "" || u.Port() == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("proxy URL must be scheme://host:port")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("proxy port is invalid")
	}
	p := &Proxy{Scheme: scheme, Host: net.JoinHostPort(u.Hostname(), u.Port())}
	if u.User != nil {
		p.Username = u.User.Username()
		p.Password, _ = u.User.Password()
		if scheme == "socks5" && (len(p.Username) > 255 || len(p.Password) > 255) {
			return nil, errors.New("SOCKS5 username and password are limited to 255 bytes")
		}
	}
	return p, nil
}

// String returns the full URL including credentials. Use Redacted for logs.
func (p *Proxy) String() string {
	if p == nil {
		return ""
	}
	u := url.URL{Scheme: p.Scheme, Host: p.Host}
	if p.Username != "" || p.Password != "" {
		u.User = url.UserPassword(p.Username, p.Password)
	}
	return u.String()
}

// Redacted returns scheme://host:port without credentials.
func (p *Proxy) Redacted() string {
	if p == nil {
		return ""
	}
	return p.Scheme + "://" + p.Host
}

// Dial connects to addr through p, or directly when p is nil.
func Dial(ctx context.Context, p *Proxy, addr string) (net.Conn, error) {
	if p == nil {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
	return p.DialContext(ctx, addr)
}

// DialContext opens a TCP tunnel to addr (host:port) through the proxy.
func (p *Proxy) DialContext(ctx context.Context, addr string) (net.Conn, error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("destination port is invalid")
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", p.Host)
	if err != nil {
		return nil, fmt.Errorf("connect to proxy %s: %w", p.Host, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	var tunneled net.Conn
	switch p.Scheme {
	case "http":
		tunneled, err = p.connectHTTP(conn, net.JoinHostPort(host, portText))
	case "socks5":
		tunneled, err = p.connectSOCKS5(conn, host, port)
	default:
		err = errors.New("unsupported proxy scheme")
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return tunneled, nil
}

func (p *Proxy) connectHTTP(conn net.Conn, addr string) (net.Conn, error) {
	request := "CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\n"
	if p.Username != "" || p.Password != "" {
		request += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(p.Username+":"+p.Password)) + "\r\n"
	}
	request += "\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		return nil, fmt.Errorf("proxy request: %w", err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return nil, fmt.Errorf("proxy response: %w", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("proxy refused the tunnel to %s: %s", addr, response.Status)
	}
	if reader.Buffered() > 0 {
		return &bufferedConn{Conn: conn, reader: reader}, nil
	}
	return conn, nil
}

func (p *Proxy) connectSOCKS5(conn net.Conn, host string, port int) (net.Conn, error) {
	methods := []byte{0x00}
	if p.Username != "" || p.Password != "" {
		methods = []byte{0x02}
	}
	if _, err := conn.Write(append([]byte{0x05, byte(len(methods))}, methods...)); err != nil {
		return nil, fmt.Errorf("SOCKS5 greeting: %w", err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return nil, fmt.Errorf("SOCKS5 greeting: %w", err)
	}
	if reply[0] != 0x05 || reply[1] == 0xff || reply[1] != methods[0] {
		return nil, errors.New("SOCKS5 proxy rejected the authentication method")
	}
	if reply[1] == 0x02 {
		auth := []byte{0x01, byte(len(p.Username))}
		auth = append(auth, p.Username...)
		auth = append(auth, byte(len(p.Password)))
		auth = append(auth, p.Password...)
		if _, err := conn.Write(auth); err != nil {
			return nil, fmt.Errorf("SOCKS5 authentication: %w", err)
		}
		if _, err := io.ReadFull(conn, reply); err != nil {
			return nil, fmt.Errorf("SOCKS5 authentication: %w", err)
		}
		if reply[1] != 0x00 {
			return nil, errors.New("SOCKS5 proxy rejected the username or password")
		}
	}
	request := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil && ip.To4() != nil {
		request = append(request, 0x01)
		request = append(request, ip.To4()...)
	} else if ip != nil {
		request = append(request, 0x04)
		request = append(request, ip.To16()...)
	} else {
		if len(host) > 255 {
			return nil, errors.New("destination host name is too long")
		}
		// Send names unresolved so the proxy resolves them on its own network.
		request = append(request, 0x03, byte(len(host)))
		request = append(request, host...)
	}
	request = binary.BigEndian.AppendUint16(request, uint16(port))
	if _, err := conn.Write(request); err != nil {
		return nil, fmt.Errorf("SOCKS5 connect: %w", err)
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, fmt.Errorf("SOCKS5 connect: %w", err)
	}
	if header[1] != 0x00 {
		return nil, fmt.Errorf("SOCKS5 proxy could not reach %s: %s", net.JoinHostPort(host, strconv.Itoa(port)), socksReplyText(header[1]))
	}
	var skip int
	switch header[3] {
	case 0x01:
		skip = 4
	case 0x04:
		skip = 16
	case 0x03:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return nil, err
		}
		skip = int(length[0])
	default:
		return nil, errors.New("SOCKS5 proxy sent an invalid reply")
	}
	if _, err := io.ReadFull(conn, make([]byte, skip+2)); err != nil {
		return nil, err
	}
	return conn, nil
}

func socksReplyText(code byte) string {
	switch code {
	case 0x01:
		return "general failure"
	case 0x02:
		return "not allowed by ruleset"
	case 0x03:
		return "network unreachable"
	case 0x04:
		return "host unreachable"
	case 0x05:
		return "connection refused"
	case 0x06:
		return "TTL expired"
	default:
		return "error " + strconv.Itoa(int(code))
	}
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.reader.Read(b) }

// Bridge copies between stdio and a tunnel to addr. It backs OpenSSH's
// ProxyCommand so ssh, scp and rsync can reach a host through the proxy.
func Bridge(ctx context.Context, p *Proxy, addr string, stdin io.Reader, stdout io.Writer) error {
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	conn, err := Dial(dialCtx, p, addr)
	cancel()
	if err != nil {
		return err
	}
	defer conn.Close()
	done := make(chan error, 2)
	go func() {
		_, err := io.Copy(conn, stdin)
		if tcp, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = tcp.CloseWrite()
		}
		done <- err
	}()
	go func() {
		_, err := io.Copy(stdout, conn)
		done <- err
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	}
}
