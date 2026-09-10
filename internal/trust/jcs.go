// Package trust verifies signed release and module manifests. It implements
// the exact subset of RFC 8785 (JSON Canonicalization Scheme) that Payesh's
// own manifests use, and Ed25519 detached-signature verification per
// docs/contracts/RELEASE_FORMAT.md.
//
// Every Payesh manifest field is a string, boolean, timestamp-as-string,
// nested object, or array of those — 64-bit quantities always cross the wire
// as decimal strings (see internal/contracts). Canonicalize deliberately
// rejects raw JSON numbers rather than implementing the ECMA-262 number
// formatting RFC 8785 otherwise requires; that avoids a well-known source of
// JCS interop bugs for a manifest shape that never needs it.
package trust

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// ErrUnsupportedNumber is returned when canonicalizing a document that
// contains a raw JSON number. Payesh manifests encode integers as decimal
// strings specifically to avoid this class of value.
var ErrUnsupportedNumber = errors.New("trust: canonicalization of raw JSON numbers is not supported")

// Canonicalize renders v (any value encoding/json can marshal) as RFC 8785
// canonical JSON bytes: UTF-8, no insignificant whitespace, object keys
// sorted by their UTF-16 code unit sequence, minimal string escaping.
func Canonicalize(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("trust: marshal: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var generic any
	if err := decoder.Decode(&generic); err != nil {
		return nil, fmt.Errorf("trust: decode: %w", err)
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, generic); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch value := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if value {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		writeCanonicalString(buf, value)
	case json.Number:
		return ErrUnsupportedNumber
	case []any:
		buf.WriteByte('[')
		for i, item := range value {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(value))
		for k := range value {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return lessUTF16(keys[i], keys[j]) })
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonicalString(buf, k)
			buf.WriteByte(':')
			if err := writeCanonical(buf, value[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("trust: unsupported canonical value type %T", v)
	}
	return nil
}

// lessUTF16 orders strings by their UTF-16 code unit sequence, as RFC 8785
// requires for object member ordering. Every field name and dynamic key used
// by Payesh manifests is ASCII, where code-unit order and Go's default
// byte-wise string order coincide, but this still converts explicitly so a
// future non-ASCII key does not silently sort incorrectly.
func lessUTF16(a, b string) bool {
	au, bu := utf16Units(a), utf16Units(b)
	for i := 0; i < len(au) && i < len(bu); i++ {
		if au[i] != bu[i] {
			return au[i] < bu[i]
		}
	}
	return len(au) < len(bu)
}

func utf16Units(s string) []uint16 {
	units := make([]uint16, 0, len(s))
	for _, r := range s {
		if r > 0xFFFF {
			r -= 0x10000
			units = append(units, uint16(0xD800+(r>>10)), uint16(0xDC00+(r&0x3FF)))
			continue
		}
		units = append(units, uint16(r))
	}
	return units
}

// writeCanonicalString writes s as an RFC 8259 JSON string using RFC 8785's
// minimal escaping: only '"', '\\', and control characters below 0x20 are
// escaped, using the short forms for \b \f \n \r \t and \u00XX otherwise.
func writeCanonicalString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(buf, `\u%04x`, r)
				continue
			}
			buf.WriteRune(r)
		}
	}
	buf.WriteByte('"')
}
