package redact

import (
	"fmt"
	"sort"
	"strings"
)

// Scrubber removes configured secret values from arbitrary text.
type Scrubber struct {
	replacer *strings.Replacer
}

// NewScrubber replaces every non-empty secret, preferring longer matches.
func NewScrubber(secrets ...Secret) *Scrubber {
	secretsByValue := make([]string, 0, len(secrets))
	seen := make(map[string]struct{}, len(secrets))
	for _, secret := range secrets {
		value := secret.Reveal()
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		secretsByValue = append(secretsByValue, value)
	}
	sort.Slice(secretsByValue, func(i, j int) bool { return len(secretsByValue[i]) > len(secretsByValue[j]) })
	values := make([]string, 0, len(secretsByValue)*2)
	for _, value := range secretsByValue {
		values = append(values, value, redacted)
	}
	return &Scrubber{replacer: strings.NewReplacer(values...)}
}

// Scrub replaces known secret values in text.
func (s *Scrubber) Scrub(text string) string {
	if s == nil || s.replacer == nil {
		return text
	}
	return s.replacer.Replace(text)
}

// Error wraps err so Error is scrubbed and error identity remains available.
func (s *Scrubber) Error(err error) error {
	if err == nil {
		return nil
	}
	return scrubbedError{message: s.Scrub(err.Error()), cause: err}
}

type scrubbedError struct {
	message string
	cause   error
}

func (e scrubbedError) Error() string { return e.message }
func (e scrubbedError) Unwrap() error { return e.cause }
func (e scrubbedError) Format(state fmt.State, verb rune) {
	if verb == 'q' {
		_, _ = fmt.Fprintf(state, "%q", e.message)
		return
	}
	_, _ = fmt.Fprint(state, e.message)
}
