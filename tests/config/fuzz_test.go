package config_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
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

func FuzzEdit_AddProvider(f *testing.F) {
	seeds, err := filepath.Glob(filepath.Join("testdata", "compat", "*", "*.yaml"))
	if err != nil {
		f.Fatal(err)
	}
	editSeeds, err := filepath.Glob(filepath.Join("testdata", "edit", "*.yaml"))
	if err != nil {
		f.Fatal(err)
	}
	for _, path := range append(seeds, editSeeds...) {
		data, err := os.ReadFile(path) // #nosec G304 -- fixtures are repository-owned testdata.
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		cfg, err := config.ParseWithEnv(data, placeholderEnv)
		if err != nil {
			t.Skip()
		}
		entry, ok := unconfiguredProvider(cfg)
		if !ok {
			t.Skip()
		}
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		doc, err := config.OpenDocument(path)
		if err != nil {
			t.Fatalf("OpenDocument rejected a valid config: %v", err)
		}
		edit := doc.Edit()
		if err := edit.AddProvider(entry); err != nil {
			t.Fatalf("AddProvider(%s): %v", entry.Name, err)
		}
		change, err := edit.Result()
		var layout *config.LayoutError
		if errors.As(err, &layout) {
			return
		}
		if err != nil {
			t.Fatalf("Result on a valid config: %v", err)
		}
		if _, err := config.ParseWithEnv(change.Content(), placeholderEnv); err != nil {
			t.Fatalf("edited config does not load: %v", err)
		}
	})
}
