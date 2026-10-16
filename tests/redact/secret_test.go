package redact_test

import (
	"bytes"
	"encoding/json"
	"errors"
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

func TestScrubber_ReplacesSecretOccurrences(t *testing.T) {
	t.Parallel()
	scrubber := redact.NewScrubber(redact.NewSecret("secret"), redact.NewSecret("secret-long"), redact.NewSecret(""))
	got := scrubber.Scrub("secret-long/secret/secret")
	if got != "[REDACTED]/[REDACTED]/[REDACTED]" {
		t.Errorf("Scrub() = %q, want every occurrence redacted", got)
	}
}

func TestScrubber_ErrorKeepsIdentityWithoutLeaking(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("provider failure: " + testKey)
	err := redact.NewScrubber(redact.NewSecret(testKey)).Error(sentinel)
	if !errors.Is(err, sentinel) {
		t.Error("scrubbed error lost its cause")
	}
	for _, verb := range []string{"%s", "%v", "%+v", "%#v", "%q"} {
		if got := fmt.Sprintf(verb, err); strings.Contains(got, testKey) {
			t.Errorf("Sprintf(%s) leaked the key: %q", verb, got)
		}
	}
}

type leakingStringer struct{ value string }

func (s leakingStringer) String() string { return s.value }

type leakingStruct struct{ value string }

func TestHandler_ScrubsMessageAndAttributes(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	key := redact.NewSecret(testKey)
	secretName := testKey
	base := slog.NewTextHandler(&buf, nil)
	logger := slog.New(redact.NewHandler(base, redact.NewScrubber(key))).With(secretName, testKey).WithGroup(secretName)
	logger.Error(testKey, secretName, testKey, "err", errors.New(testKey), "stringer", leakingStringer{value: testKey}, "struct", leakingStruct{value: testKey}, "nested", slog.Group(secretName, "key", testKey))

	if strings.Contains(buf.String(), testKey) {
		t.Fatalf("log output contains key: %s", buf.String())
	}
	if got := strings.Count(buf.String(), "[REDACTED]"); got < 7 {
		t.Errorf("log output has %d redactions, want at least 7: %s", got, buf.String())
	}
}

func TestHandler_ScrubsAttributesAtEveryLevel(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		level slog.Level
		log   func(*slog.Logger, string, ...any)
	}{
		{name: "debug", level: slog.LevelDebug, log: (*slog.Logger).Debug},
		{name: "info", level: slog.LevelInfo, log: (*slog.Logger).Info},
		{name: "warn", level: slog.LevelWarn, log: (*slog.Logger).Warn},
		{name: "error", level: slog.LevelError, log: (*slog.Logger).Error},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: tt.level})
			logger := slog.New(redact.NewHandler(handler, redact.NewScrubber(redact.NewSecret(testKey))))

			tt.log(logger, "provider response", "api_key", testKey)

			if strings.Contains(buf.String(), testKey) || !strings.Contains(buf.String(), "api_key=[REDACTED]") {
				t.Errorf("log output = %q, want the provider key redacted", buf.String())
			}
		})
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
