package redact_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/Polqt/spillway/internal/redact"
)

const testKey = "sk-test-redact-0123456789"

type holder struct {
	Key redact.Secret
}

func shapes() map[string]any {
	s := redact.NewSecret(testKey)
	return map[string]any{
		"value":   s,
		"pointer": &s,
		"struct":  holder{Key: s},
		"slice":   []redact.Secret{s},
		"map":     map[string]redact.Secret{"k": s},
	}
}

func TestSecret_FmtNeverRevealsValue(t *testing.T) {
	t.Parallel()
	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"}
	for name, v := range shapes() {
		for _, verb := range verbs {
			t.Run(name+" "+verb, func(t *testing.T) {
				t.Parallel()
				got := fmt.Sprintf(verb, v)
				if strings.Contains(got, testKey) {
					t.Fatalf("Sprintf(%s) = %q, contains the key", verb, got)
				}
				if !strings.Contains(got, "[REDACTED]") {
					t.Errorf("Sprintf(%s) = %q, want [REDACTED]", verb, got)
				}
			})
		}
	}
}

func TestSecret_JSONNeverRevealsValue(t *testing.T) {
	t.Parallel()
	cases := shapes()
	cases["map key"] = map[redact.Secret]int{redact.NewSecret(testKey): 1}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if bytes.Contains(b, []byte(testKey)) {
				t.Fatalf("Marshal = %s, contains the key", b)
			}
			if !bytes.Contains(b, []byte("[REDACTED]")) {
				t.Errorf("Marshal = %s, want [REDACTED]", b)
			}
		})
	}
}

func TestSecret_SlogNeverRevealsValue(t *testing.T) {
	t.Parallel()
	handlers := map[string]func(*bytes.Buffer) slog.Handler{
		"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
		"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
	}
	s := redact.NewSecret(testKey)
	for name, newHandler := range handlers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			logger := slog.New(newHandler(&buf))

			logger.Info("direct", "key", s)
			logger.Info("group", slog.Group("g", "key", s))
			logger.Info("struct", "holder", holder{Key: s})
			logger.Info("pointer", "key", &s)

			if strings.Contains(buf.String(), testKey) {
				t.Fatalf("log output contains the key:\n%s", buf.String())
			}
			if got := strings.Count(buf.String(), "[REDACTED]"); got < 4 {
				t.Errorf("log output has %d [REDACTED], want at least 4:\n%s", got, buf.String())
			}
		})
	}
}

func TestSecret_RevealReturnsValue(t *testing.T) {
	t.Parallel()
	if got := redact.NewSecret(testKey).Reveal(); got != testKey {
		t.Errorf("Reveal() = %q, want %q", got, testKey)
	}
}

func TestSecret_UnmarshalTextRoundTrip(t *testing.T) {
	t.Parallel()
	var s redact.Secret
	if err := s.UnmarshalText([]byte(testKey)); err != nil {
		t.Fatalf("UnmarshalText: %v", err)
	}
	if s.Reveal() != testKey {
		t.Errorf("Reveal() = %q, want %q", s.Reveal(), testKey)
	}
}

func TestSecret_ZeroValue(t *testing.T) {
	t.Parallel()
	var zero redact.Secret
	if !zero.IsZero() {
		t.Error("zero Secret: IsZero() = false, want true")
	}
	if zero.String() != "" {
		t.Errorf("zero Secret: String() = %q, want empty", zero.String())
	}
	if redact.NewSecret(testKey).IsZero() {
		t.Error("non-zero Secret: IsZero() = true, want false")
	}
}

// TestSecret_UnexportedFieldLimitation documents why Secret fields must be
// exported: fmt cannot call methods on unexported fields, so the raw value
// prints under %+v.
func TestSecret_UnexportedFieldLimitation(t *testing.T) {
	t.Parallel()
	type hidden struct{ key redact.Secret }

	got := fmt.Sprintf("%+v", hidden{key: redact.NewSecret(testKey)})

	if !strings.Contains(got, testKey) {
		t.Errorf("Sprintf = %q; fmt now redacts unexported fields, update the redact package comment", got)
	}
}
