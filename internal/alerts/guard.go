package alerts

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

var errBlockedAddress = errors.New("destination address is not allowed")

// metadataAddresses are cloud instance-metadata endpoints. They are never
// reachable through a destination, even when private networks are allowed,
// because they hand out instance credentials.
var metadataAddresses = []netip.Addr{
	netip.MustParseAddr("169.254.169.254"),
	netip.MustParseAddr("169.254.170.2"),
	netip.MustParseAddr("100.100.100.200"),
	netip.MustParseAddr("fd00:ec2::254"),
}

func isMetadataAddress(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, blocked := range metadataAddresses {
		if addr == blocked {
			return true
		}
	}
	return false
}

// isPrivateAddress reports whether addr is anything other than an ordinary
// public unicast address: loopback, private, link-local, CGNAT, unspecified,
// multicast, or documentation-only space.
func isPrivateAddress(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return true
	}
	if addr.Is4() {
		b := addr.As4()
		// 100.64.0.0/10 shared (CGNAT) space, 0.0.0.0/8, 192.0.0.0/24, 198.18.0.0/15.
		if b[0] == 100 && b[1]&0xc0 == 64 || b[0] == 0 || b[0] == 192 && b[1] == 0 && b[2] == 0 || b[0] == 198 && (b[1] == 18 || b[1] == 19) {
			return true
		}
	}
	return false
}

// checkDialAddress is the single policy for where a notification may connect.
// metadata addresses are always refused; private space needs the owner opt-in;
// plain HTTP additionally never reaches a public address.
func checkDialAddress(addr netip.Addr, allowPrivate, plainHTTP bool) error {
	if isMetadataAddress(addr) {
		return errBlockedAddress
	}
	private := isPrivateAddress(addr)
	if private && !allowPrivate {
		return errBlockedAddress
	}
	if plainHTTP && !private {
		return errBlockedAddress
	}
	return nil
}

// guardedClient builds an HTTP client that re-checks every resolved address at
// connect time, so a hostname that later resolves to an internal address (DNS
// rebinding) is refused. Redirects are never followed.
func guardedClient(allowPrivate, plainHTTP bool, timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return errBlockedAddress
			}
			addr, err := netip.ParseAddr(host)
			if err != nil {
				return errBlockedAddress
			}
			return checkDialAddress(addr, allowPrivate, plainHTTP)
		},
	}
	transport := &http.Transport{
		// Notifications must never be routed through an ambient proxy: the
		// connect-time address check would then apply to the proxy, not the
		// destination.
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
		DisableCompression:    true,
	}
	return &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// validateDestinationURL statically checks a destination URL. Literal IP
// hosts are checked against the same address policy; hostnames are checked at
// connect time by the guarded client.
func validateDestinationURL(raw string, allowHTTP, allowPrivate bool) error {
	if len(raw) == 0 || len(raw) > 2048 {
		return errors.New("notification URL is outside its length bound")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.Hostname() == "" {
		return errors.New("notification URL must be an absolute URL without credentials or fragments")
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		if !allowHTTP {
			return errors.New("notification URL must use HTTPS unless plain HTTP is explicitly allowed")
		}
		if !allowPrivate {
			return errors.New("plain HTTP is only allowed together with private network access")
		}
	default:
		return errors.New("notification URL must use HTTPS unless plain HTTP is explicitly allowed")
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if addr, err := netip.ParseAddr(host); err == nil {
		if err := checkDialAddress(addr, allowPrivate, parsed.Scheme == "http"); err != nil {
			return errBlockedAddress
		}
	} else if !allowPrivate && (host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".local")) {
		return errBlockedAddress
	}
	return nil
}

// forbiddenHeaders cannot be set by an owner: they would corrupt framing or
// forge Payesh's own headers.
var forbiddenHeaders = map[string]bool{
	"host": true, "content-length": true, "transfer-encoding": true, "connection": true, "content-type": true,
	"content-encoding": true, "te": true, "trailer": true, "upgrade": true, "proxy-authorization": true,
	"x-payesh-signature": true, "x-payesh-event": true, "x-payesh-delivery": true, "user-agent": true,
}

const (
	maxDestinationHeaders     = 10
	maxDestinationHeaderValue = 1024
)

func validHeaderName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// ValidateHeaders checks owner-supplied static headers.
func ValidateHeaders(headers map[string]string) error {
	if len(headers) > maxDestinationHeaders {
		return errors.New("at most 10 custom headers are allowed")
	}
	for name, value := range headers {
		if !validHeaderName(name) || forbiddenHeaders[strings.ToLower(name)] {
			return errors.New("custom header name is not allowed")
		}
		if len(value) == 0 || len(value) > maxDestinationHeaderValue || strings.ContainsAny(value, "\r\n\x00") {
			return errors.New("custom header value is empty, too long, or contains control characters")
		}
	}
	return nil
}
