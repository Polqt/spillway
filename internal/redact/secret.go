// Package redact keeps secrets out of every output channel.
//
// A Secret prints as [REDACTED] under fmt, encoding/json, encoding.TextMarshaler
// (YAML, slog text) and slog.LogValuer. Reveal is the only way to read the value.
//
// Known limitation: fmt cannot call methods on unexported struct fields, so a
// Secret stored in an unexported field prints raw under %+v. Secret fields are
// therefore always exported.
package redact

import (
	"fmt"
	"log/slog"
	"strconv"
)

const redacted = "[REDACTED]"

// Secret holds a credential that must never be printed, logged or encoded.
// The zero value is an empty secret.
type Secret struct {
	v string
}

// NewSecret wraps s.
func NewSecret(s string) Secret {
	return Secret{v: s}
}

// Reveal returns the raw value. It is the only accessor, so every place a
// secret leaves the type can be found with grep.
func (s Secret) Reveal() string {
	return s.v
}

// IsZero reports whether the secret is empty.
func (s Secret) IsZero() bool {
	return s.v == ""
}

// String returns "[REDACTED]", or "" for an empty secret.
func (s Secret) String() string {
	if s.IsZero() {
		return ""
	}
	return redacted
}

// GoString implements fmt.GoStringer for %#v.
func (s Secret) GoString() string {
	return "redact.Secret(" + strconv.Quote(s.String()) + ")"
}

// Format implements fmt.Formatter so that every verb, including %x and %q,
// prints the redacted form.
func (s Secret) Format(f fmt.State, verb rune) {
	var out string
	switch {
	case verb == 'v' && f.Flag('#'):
		out = s.GoString()
	case verb == 'q':
		out = strconv.Quote(s.String())
	default:
		out = s.String()
	}
	_, _ = f.Write([]byte(out))
}

// MarshalText implements encoding.TextMarshaler. YAML, slog text output and
// JSON map keys use it.
func (s Secret) MarshalText() ([]byte, error) {
	return []byte(s.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *Secret) UnmarshalText(b []byte) error {
	s.v = string(b)
	return nil
}

// MarshalJSON implements json.Marshaler.
func (s Secret) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(s.String())), nil
}

// LogValue implements slog.LogValuer.
func (s Secret) LogValue() slog.Value {
	return slog.StringValue(s.String())
}
