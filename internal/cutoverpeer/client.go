package cutoverpeer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
	"github.com/Real-kia/payesh/internal/monitoring"
)

// Client is the source side of the peer. It implements updater.CutoverPeer, so a
// StoreCutover runs unchanged against a remote destination.
type Client struct {
	BaseURL string
	// ServerCertSHA256 pins the destination's leaf certificate. Pinning replaces
	// chain verification: a certificate that does not hash to this value is refused.
	ServerCertSHA256 string
	ClientCert       tls.Certificate
	// ServerID is the one server this client may act on; calls for any other
	// server are refused locally before reaching the network.
	ServerID   contracts.ServerID
	ChunkSize  int
	MaxRetries int
	RetryDelay time.Duration
	// HTTPClient overrides the pinned TLS client, for tests only.
	HTTPClient *http.Client

	once   sync.Once
	client *http.Client
}

func (c *Client) http() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	c.once.Do(func() {
		pin := strings.ToLower(c.ServerCertSHA256)
		config := &tls.Config{
			MinVersion: tls.VersionTLS13,
			// The chain is not verified; the pinned fingerprint below is the trust anchor.
			InsecureSkipVerify: true, //nolint:gosec // replaced by certificate pinning
			VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
				if len(raw) == 0 {
					return errors.New("cutoverpeer: the server presented no certificate")
				}
				sum := sha256.Sum256(raw[0])
				if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(pin)) != 1 {
					return errPinMismatch
				}
				return nil
			},
		}
		if len(c.ClientCert.Certificate) > 0 {
			config.Certificates = []tls.Certificate{c.ClientCert}
		}
		c.client = &http.Client{Timeout: 5 * time.Minute, Transport: &http.Transport{TLSClientConfig: config, MaxIdleConnsPerHost: 2}}
	})
	return c.client
}

func (c *Client) retries() int {
	if c.MaxRetries > 0 {
		return c.MaxRetries
	}
	return 4
}

func (c *Client) delay() time.Duration {
	if c.RetryDelay > 0 {
		return c.RetryDelay
	}
	return 250 * time.Millisecond
}

var errPinMismatch = errors.New("cutoverpeer: the server certificate does not match its pin")

// permanent reports transport failures that retrying cannot fix: a certificate
// pin mismatch, or the server rejecting our TLS credentials during the handshake.
func permanent(err error) bool {
	return errors.Is(err, errPinMismatch) || strings.Contains(err.Error(), "remote error: tls:")
}

type peerError struct {
	status int
	body   string
}

func (e *peerError) Error() string {
	return fmt.Sprintf("cutover peer returned %d: %s", e.status, e.body)
}

// do sends one request, retrying only transient failures (network errors and 5xx).
// Client errors such as 403 can never succeed on retry and are returned at once.
func (c *Client) do(ctx context.Context, method, path string, header map[string]string, body []byte) ([]byte, error) {
	var last error
	for attempt := 0; attempt <= c.retries(); attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(c.delay() * time.Duration(attempt)):
			}
		}
		request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		for key, value := range header {
			request.Header.Set(key, value)
		}
		response, err := c.http().Do(request)
		if err != nil {
			if permanent(err) {
				return nil, err
			}
			last = err
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		_ = response.Body.Close()
		if readErr != nil {
			last = readErr
			continue
		}
		if response.StatusCode >= 500 {
			last = &peerError{status: response.StatusCode, body: strings.TrimSpace(string(data))}
			continue
		}
		if response.StatusCode >= 400 {
			return nil, &peerError{status: response.StatusCode, body: strings.TrimSpace(string(data))}
		}
		return data, nil
	}
	return nil, fmt.Errorf("cutover peer unreachable after %d attempts: %w", c.retries()+1, last)
}

func (c *Client) jsonCall(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	header := map[string]string{}
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, header["Content-Type"] = encoded, "application/json"
	}
	data, err := c.do(ctx, method, path, header, body)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (c *Client) requireServer(serverID contracts.ServerID) error {
	if c.ServerID == "" || serverID != c.ServerID {
		return errors.New("cutover peer: the client is not configured for this server")
	}
	return nil
}

// ensureUploaded makes the artifact complete on the peer, sending only the
// chunks the peer does not already hold, and returns its identifier.
func (c *Client) ensureUploaded(ctx context.Context, data []byte) (string, error) {
	if len(data) == 0 || len(data) > maxArtifactBytes {
		return "", errors.New("cutover peer: artifact size is out of range")
	}
	sum := sha256.Sum256(data)
	id := hex.EncodeToString(sum[:])
	for attempt := 0; attempt < 2; attempt++ {
		var status artifactStatusWire
		if err := c.jsonCall(ctx, http.MethodGet, "/v1/cutover/artifact/"+id, nil, &status); err != nil {
			return "", err
		}
		if status.Complete {
			return id, nil
		}
		size := c.ChunkSize
		if size <= 0 {
			size = 1 << 20
		}
		if need := (len(data) + maxChunks - 1) / maxChunks; size < need {
			size = need
		}
		if size > maxChunkBytes {
			return "", errors.New("cutover peer: artifact cannot be chunked within the limits")
		}
		have := make(map[int]bool, len(status.Chunks))
		for _, index := range status.Chunks {
			have[index] = true
		}
		chunks := (len(data) + size - 1) / size
		for index := 0; index < chunks; index++ {
			if have[index] {
				continue
			}
			end := min((index+1)*size, len(data))
			part := data[index*size : end]
			partSum := sha256.Sum256(part)
			if _, err := c.do(ctx, http.MethodPut, "/v1/cutover/artifact/"+id+"/chunk/"+strconv.Itoa(index), map[string]string{chunkHashHeader: hex.EncodeToString(partSum[:])}, part); err != nil {
				return "", err
			}
		}
		err := c.jsonCall(ctx, http.MethodPost, "/v1/cutover/artifact/"+id+"/commit", commitWire{Chunks: chunks, Size: len(data)}, nil)
		var refused *peerError
		if errors.As(err, &refused) && refused.status == http.StatusConflict {
			continue // a spool chunk vanished; re-read the status and resend it
		}
		if err != nil {
			return "", err
		}
		return id, nil
	}
	return "", errors.New("cutover peer: the artifact could not be completed")
}

// ImportArtifact uploads the artifact (and its baseline, when given) and imports it.
func (c *Client) ImportArtifact(ctx context.Context, artifact, previous []byte) error {
	id, err := c.ensureUploaded(ctx, artifact)
	if err != nil {
		return err
	}
	request := importWire{ArtifactSHA256: id}
	if len(previous) > 0 {
		if request.PreviousSHA256, err = c.ensureUploaded(ctx, previous); err != nil {
			return err
		}
	}
	return c.jsonCall(ctx, http.MethodPost, "/v1/cutover/import", request, &importResultWire{})
}

func (c *Client) status(ctx context.Context, serverID contracts.ServerID) (serverStatusWire, error) {
	var status serverStatusWire
	if err := c.requireServer(serverID); err != nil {
		return status, err
	}
	return status, c.jsonCall(ctx, http.MethodGet, "/v1/cutover/server", nil, &status)
}

func (c *Client) HasServer(ctx context.Context, serverID contracts.ServerID) (bool, error) {
	status, err := c.status(ctx, serverID)
	return status.HasServer, err
}

func (c *Client) GetAuthority(ctx context.Context, serverID contracts.ServerID) (monitoring.ServerAuthority, bool, error) {
	status, err := c.status(ctx, serverID)
	if err != nil || !status.Found {
		return monitoring.ServerAuthority{}, false, err
	}
	return fromWire(status.Authority), true, nil
}

func (c *Client) ActivateAuthority(ctx context.Context, handoff monitoring.AuthorityHandoff) (monitoring.ServerAuthority, error) {
	if err := c.requireServer(handoff.ServerID); err != nil {
		return monitoring.ServerAuthority{}, err
	}
	var out authorityWire
	err := c.jsonCall(ctx, http.MethodPost, "/v1/cutover/activate", handoffWire{ServerID: string(handoff.ServerID), CutoverID: handoff.CutoverID,
		SourceGeneration: handoff.SourceGeneration, FrontierDigest: handoff.FrontierDigest, SourceRequestDigest: handoff.SourceRequestDigest, NewOwner: handoff.NewOwner}, &out)
	if err != nil {
		return monitoring.ServerAuthority{}, err
	}
	return fromWire(out), nil
}
