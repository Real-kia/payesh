package transport

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

const maxTransportFrameBytes = contracts.MaxEnvelopeBytes

var (
	ErrFrameTooLarge    = errors.New("transport: frame exceeds maximum size")
	ErrFrameMalformed   = errors.New("transport: frame is malformed")
	ErrFrameUnsupported = errors.New("transport: frame type is unsupported")
)

// Frame is the bounded payload exchanged by the future TLS/WebSocket adapter.
// The outer four-byte length prefix is transport-neutral, so the same codec
// can be used over a websocket binary message or a reconnecting stream test.
type Frame struct {
	Protocol string          `json:"protocol"`
	Type     string          `json:"type"`
	Request  string          `json:"request_id,omitempty"`
	Body     json.RawMessage `json:"body,omitempty"`
}

var frameTypes = map[string]struct{}{
	"hello": {}, "heartbeat": {}, "sample_batch": {}, "acknowledgement": {},
	"action_request": {}, "action_response": {}, "cancellation": {}, "error": {},
}

func (f Frame) Validate() error {
	if f.Protocol != contracts.NodeProtocol && f.Protocol != contracts.HelperProtocol && f.Protocol != contracts.ModuleProtocol {
		return fmt.Errorf("%w: protocol", ErrFrameMalformed)
	}
	if _, ok := frameTypes[f.Type]; !ok {
		return fmt.Errorf("%w: %q", ErrFrameUnsupported, f.Type)
	}
	if len(f.Type) > 64 || len(f.Request) > 128 || len(f.Body) > maxTransportFrameBytes {
		return ErrFrameTooLarge
	}
	if len(f.Body) == 0 && f.Type != "heartbeat" {
		return fmt.Errorf("%w: body is required", ErrFrameMalformed)
	}
	return nil
}

// WriteFrame writes one complete length-delimited frame. It never writes a
// partial frame after JSON encoding fails and never accepts a body larger than
// the shared envelope limit.
func WriteFrame(w io.Writer, frame Frame) error {
	if w == nil {
		return fmt.Errorf("%w: nil writer", ErrFrameMalformed)
	}
	if err := frame.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("transport: encode frame: %w", err)
	}
	if len(payload) > maxTransportFrameBytes {
		return ErrFrameTooLarge
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(payload)))
	if _, err := writeFull(w, length[:]); err != nil {
		return fmt.Errorf("transport: write frame length: %w", err)
	}
	if _, err := writeFull(w, payload); err != nil {
		return fmt.Errorf("transport: write frame body: %w", err)
	}
	return nil
}

// ReadFrame reads one complete frame with a hard allocation bound. Partial
// reads, EOF, truncated payloads, invalid JSON, and invalid protocol fields
// all return explicit errors for the reconnect loop to classify.
func ReadFrame(r io.Reader) (Frame, error) {
	if r == nil {
		return Frame{}, fmt.Errorf("%w: nil reader", ErrFrameMalformed)
	}
	var length [4]byte
	if _, err := io.ReadFull(r, length[:]); err != nil {
		return Frame{}, fmt.Errorf("transport: read frame length: %w", err)
	}
	size := binary.BigEndian.Uint32(length[:])
	if size == 0 || size > maxTransportFrameBytes {
		return Frame{}, ErrFrameTooLarge
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return Frame{}, fmt.Errorf("transport: read frame body: %w", err)
	}
	var frame Frame
	dec := json.NewDecoder(bytesReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&frame); err != nil {
		return Frame{}, fmt.Errorf("%w: %v", ErrFrameMalformed, err)
	}
	var trailing struct{}
	if err := dec.Decode(&trailing); err != io.EOF {
		return Frame{}, fmt.Errorf("%w: trailing data", ErrFrameMalformed)
	}
	if err := frame.Validate(); err != nil {
		return Frame{}, err
	}
	return frame, nil
}

// ReconnectBackoff produces bounded exponential delays. Jitter is injected so
// production can randomize reconnect storms while tests remain deterministic.
type ReconnectBackoff struct {
	Attempt int
	Base    time.Duration
	Max     time.Duration
	Jitter  func(time.Duration) time.Duration
}

func (b *ReconnectBackoff) Next() time.Duration {
	base, max := b.Base, b.Max
	if base <= 0 {
		base = time.Second
	}
	if max <= 0 {
		max = 60 * time.Second
	}
	if max < base {
		max = base
	}
	shift := b.Attempt
	if shift < 0 {
		shift = 0
	}
	if shift > 30 {
		shift = 30
	}
	delay := base
	if shift > 0 && delay > max/time.Duration(uint64(1)<<shift) {
		delay = max
	} else {
		delay *= time.Duration(uint64(1) << shift)
		if delay > max || delay <= 0 {
			delay = max
		}
	}
	b.Attempt++
	if b.Jitter != nil {
		delay = b.Jitter(delay)
		if delay < 0 {
			delay = 0
		}
		if delay > max {
			delay = max
		}
	}
	return delay
}

func (b *ReconnectBackoff) Reset() { b.Attempt = 0 }

func writeFull(w io.Writer, data []byte) (int, error) {
	total := 0
	for len(data) > 0 {
		n, err := w.Write(data)
		if n < 0 || n > len(data) {
			return total, io.ErrShortWrite
		}
		total += n
		data = data[n:]
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

// bytesReader avoids exposing a mutable bytes.Buffer to callers and keeps the
// decoder's input strictly bounded by the already allocated frame payload.
func bytesReader(data []byte) io.Reader { return &sliceReader{data: data} }

type sliceReader struct {
	data []byte
	off  int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.off == len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}
