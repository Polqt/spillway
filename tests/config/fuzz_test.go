package config_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Polqt/spillway/internal/config"
)

const fuzzEnvMarker = "spillway-fuzz-secret-73b5f1c2"

func FuzzParseWithEnv(f *testing.F) {
	f.Add([]byte("server:\n  api_keys: [\"${SPILLWAY_KEY}\"]\nproviders:\n  mock: {}\n"))
	f.Add([]byte("server: [malformed"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if bytes.Contains(data, []byte(fuzzEnvMarker)) {
			t.Skip()
		}
		_, err := config.ParseWithEnv(data, func(string) (string, bool) {
			return fuzzEnvMarker, true
		})
		if err != nil && strings.Contains(err.Error(), fuzzEnvMarker) {
			t.Fatalf("ParseWithEnv error exposes an environment value: %v", err)
		}
	})
}
