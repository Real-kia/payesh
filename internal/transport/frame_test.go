package transport

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

func TestFrameRoundTripHandlesPartialWrites(t *testing.T) {
	var wire partialWriter
	want := Frame{Protocol: contracts.NodeProtocol, Type: "heartbeat"}
	if err := WriteFrame(&wire, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFrame(bytes.NewReader(wire.data))
	if err != nil {
		t.Fatal(err)
	}
	if got.Protocol != want.Protocol || got.Type != want.Type {
		t.Fatalf("frame=%+v, want=%+v", got, want)
	}
}

func TestFrameRejectsOversizeAndTrailingData(t *testing.T) {
	var wire bytes.Buffer
	oversizedJSON := []byte("[" + strings.Repeat("1,", contracts.MaxEnvelopeBytes/2) + "1]")
	if err := WriteFrame(&wire, Frame{Protocol: contracts.NodeProtocol, Type: "hello", Body: oversizedJSON}); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversize write err=%v", err)
	}
	payload := []byte(`{"protocol":"payesh.node.v1","type":"heartbeat"} {"extra":true}`)
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	input := append(header[:], payload...)
	if _, err := ReadFrame(bytes.NewReader(input)); !errors.Is(err, ErrFrameMalformed) {
		t.Fatalf("trailing data err=%v", err)
	}
}

func TestFrameRejectsTruncatedAndUnknownType(t *testing.T) {
	if _, err := ReadFrame(bytes.NewReader([]byte{0, 0, 0, 4, '{'})); err == nil {
		t.Fatal("truncated frame accepted")
	}
	if err := (Frame{Protocol: contracts.NodeProtocol, Type: "unknown", Body: []byte(`{}`)}).Validate(); !errors.Is(err, ErrFrameUnsupported) {
		t.Fatalf("unknown type err=%v", err)
	}
}

func TestReconnectBackoffIsCappedAndResettable(t *testing.T) {
	b := ReconnectBackoff{Base: time.Second, Max: 5 * time.Second}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second}
	for _, expected := range want {
		if got := b.Next(); got != expected {
			t.Fatalf("delay=%s, want=%s", got, expected)
		}
	}
	b.Reset()
	if got := b.Next(); got != time.Second {
		t.Fatalf("reset delay=%s", got)
	}
	b.Jitter = func(delay time.Duration) time.Duration { return delay + time.Second }
	if got := b.Next(); got != 3*time.Second {
		t.Fatalf("jitter delay=%s", got)
	}
}

type partialWriter struct{ data []byte }

func (w *partialWriter) Write(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	w.data = append(w.data, p...)
	return len(p), nil
}

var _ io.Writer = (*partialWriter)(nil)
