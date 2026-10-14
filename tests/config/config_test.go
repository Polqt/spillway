package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Polqt/spillway/internal/config"
)

const (
	localKeyCI     = "sk-test-local-ci"
	localKeyCursor = "sk-test-local-cursor"
)

func TestParse_Valid(t *testing.T) {
	t.Parallel()
	cfg, err := config.Parse([]byte(`
server:
  api_keys:
    - name: demo
      key: sk-test-local-ci
providers:
  mock: {}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Server.Listen != "127.0.0.1:8080" {
		t.Errorf("Listen = %q, want default 127.0.0.1:8080", cfg.Server.Listen)
	}
	if len(cfg.Server.APIKeys) != 1 || cfg.Server.APIKeys[0].Name != "demo" || cfg.Server.APIKeys[0].Key.Reveal() != localKeyCI {
		t.Errorf("APIKeys = %+v, want one key named demo", cfg.Server.APIKeys)
	}
	if _, ok := cfg.Providers["mock"]; !ok || len(cfg.Providers) != 1 {
		t.Errorf("Providers = %v, want only mock", cfg.Providers)
	}
}

func TestParse_LocalKeyForms(t *testing.T) {
	t.Parallel()
	type want struct{ name, value string }
	tests := []struct {
		name    string
		entries string
		want    []want
	}{
		{name: "one plain", entries: `["a"]`, want: []want{{"key-1", "a"}}},
		{name: "two plain", entries: `["a", "b"]`, want: []want{{"key-1", "a"}, {"key-2", "b"}}},
		{name: "named", entries: `[{name: ci, key: a}]`, want: []want{{"ci", "a"}}},
		{
			name:    "mixed counts every position",
			entries: `["a", {name: ci, key: b}, "c"]`,
			want:    []want{{"key-1", "a"}, {"ci", "b"}, {"key-3", "c"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Parse([]byte("server:\n  api_keys: " + tt.entries + "\nproviders:\n  mock: {}\n"))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(cfg.Server.APIKeys) != len(tt.want) {
				t.Fatalf("got %d keys, want %d", len(cfg.Server.APIKeys), len(tt.want))
			}
			for i, w := range tt.want {
				got := cfg.Server.APIKeys[i]
				if got.Name != w.name || got.Key.Reveal() != w.value {
					t.Errorf("key %d = {%s, %s}, want {%s, %s}", i, got.Name, got.Key.Reveal(), w.name, w.value)
				}
			}
		})
	}
}

func TestParse_LocalKeyErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		entries string
		want    string
	}{
		{name: "missing", entries: "", want: "server.api_keys must list at least one local key"},
		{name: "empty list", entries: "  api_keys: []\n", want: "server.api_keys must list at least one local key"},
		{name: "scalar", entries: "  api_keys: sk-test-local-ci\n", want: "server.api_keys (line 2): must be a list"},
		{name: "nested list", entries: "  api_keys: [[sk-test-local-ci]]\n", want: "server.api_keys[0] (line 2)"},
		{name: "null entry", entries: "  api_keys: [null]\n", want: "server.api_keys[0] (line 2)"},
		{name: "mapping without name", entries: "  api_keys: [{key: sk-test-local-ci}]\n", want: "server.api_keys[0].name is required"},
		{name: "mapping without key", entries: "  api_keys: [{name: ci}]\n", want: "server.api_keys[0].key is required"},
		{name: "unknown field", entries: "  api_keys: [{name: ci, key: sk-test-local-ci, scope: all}]\n", want: `server.api_keys[0] (line 2): unknown field`},
		// Security property: a key mistyped as a mapping key is never printed.
		{name: "key as mapping key with null value", entries: "  api_keys:\n    - sk-test-local-ci:\n", want: "server.api_keys[0] (line 3): unknown field"},
		{name: "key as mapping key with a value", entries: "  api_keys: [{sk-test-local-ci: x}]\n", want: "server.api_keys[0] (line 2): unknown field"},
		{name: "key as extra field", entries: "  api_keys: [{name: ci, key: sk-test-local-cursor, sk-test-local-ci: 1}]\n", want: "server.api_keys[0] (line 2): unknown field"},
		{name: "non-string name", entries: "  api_keys: [{name: [ci], key: sk-test-local-ci}]\n", want: "server.api_keys[0].name (line 2): must be a string"},
		{name: "empty key", entries: "  api_keys: [\"\"]\n", want: "server.api_keys[0].key must not be empty"},
		{name: "key with space", entries: "  api_keys: [\"sk-test-local ci\"]\n", want: "server.api_keys[0].key must not contain whitespace"},
		{name: "uppercase name", entries: "  api_keys: [{name: CI, key: sk-test-local-ci}]\n", want: `server.api_keys[0].name "CI" must be`},
		{name: "name with space", entries: "  api_keys: [{name: my app, key: sk-test-local-ci}]\n", want: `server.api_keys[0].name "my app" must be`},
		{name: "name with underscore", entries: "  api_keys: [{name: app_1, key: sk-test-local-ci}]\n", want: `server.api_keys[0].name "app_1" must be`},
		{name: "empty name", entries: "  api_keys: [{name: \"\", key: sk-test-local-ci}]\n", want: `server.api_keys[0].name "" must be`},
		{name: "65 character name", entries: "  api_keys: [{name: " + strings.Repeat("a", 65) + ", key: sk-test-local-ci}]\n", want: "server.api_keys[0].name"},
		{
			name:    "duplicate names",
			entries: "  api_keys: [{name: ci, key: sk-test-local-ci}, {name: ci, key: sk-test-local-cursor}]\n",
			want:    `server.api_keys[1].name "ci" duplicates server.api_keys[0]`,
		},
		{
			name:    "explicit name collides with generated",
			entries: "  api_keys: [sk-test-local-ci, sk-test-local-cursor, {name: key-2, key: sk-test-local-x}]\n",
			want:    `server.api_keys[2].name "key-2" duplicates server.api_keys[1]`,
		},
		{
			name:    "same key under two names",
			entries: "  api_keys: [{name: ci, key: sk-test-local-ci}, {name: cursor, key: sk-test-local-ci}]\n",
			want:    "server.api_keys[1].key duplicates server.api_keys[0].key",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Parse([]byte("server:\n" + tt.entries + "providers:\n  mock: {}\n"))
			if err == nil {
				t.Fatal("Parse succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want substring %q", err, tt.want)
			}
			for _, key := range []string{localKeyCI, localKeyCursor, "sk-test-local ci"} {
				if strings.Contains(err.Error(), key) {
					t.Errorf("error = %q, contains key %q", err, key)
				}
			}
		})
	}
}

// Security property: no listen address, loopback included, accepts a config
// without a local key (ADR 0003).
func TestParse_LocalKeyRequiredOnEveryListen(t *testing.T) {
	t.Parallel()
	listens := []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080", "0.0.0.0:8080", ":8080", "[::]:8080", "192.168.1.5:8080"}
	for _, listen := range listens {
		t.Run(listen, func(t *testing.T) {
			t.Parallel()
			_, err := config.Parse([]byte(fmt.Sprintf("server:\n  listen: %q\nproviders:\n  mock: {}\n", listen)))
			if err == nil {
				t.Fatal("Parse without a local key succeeded, want error")
			}
			for _, want := range []string{"server.api_keys", "ADR 0003"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want substring %q", err, want)
				}
			}

			_, err = config.Parse([]byte(fmt.Sprintf("server:\n  listen: %q\n  api_keys: [sk-test-local-ci]\nproviders:\n  mock: {}\n", listen)))
			if err != nil {
				t.Errorf("Parse with a local key: %v", err)
			}
		})
	}
}

func TestServer_Loopback(t *testing.T) {
	t.Parallel()
	tests := []struct {
		listen string
		want   bool
	}{
		{"127.0.0.1:8080", true},
		{"127.0.0.2:8080", true},
		{"localhost:8080", true},
		{"LOCALHOST:8080", true},
		{"[::1]:8080", true},
		{":8080", false},
		{"0.0.0.0:8080", false},
		{"[::]:8080", false},
		{"192.168.1.5:8080", false},
		{"example.com:8080", false},
	}
	for _, tt := range tests {
		t.Run(tt.listen, func(t *testing.T) {
			t.Parallel()
			if got := (config.Server{Listen: tt.listen}).Loopback(); got != tt.want {
				t.Errorf("Loopback() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestServer_Host(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"127.0.0.1:8080": "127.0.0.1",
		"[::1]:8080":     "::1",
		"localhost:8080": "localhost",
		":8080":          "",
		"nope":           "",
	}
	for listen, want := range tests {
		t.Run(listen, func(t *testing.T) {
			t.Parallel()
			if got := (config.Server{Listen: listen}).Host(); got != want {
				t.Errorf("Host() = %q, want %q", got, want)
			}
		})
	}
}

func TestParse_Errors(t *testing.T) {
	t.Parallel()
	const keys = "  api_keys: [sk-test-local-ci]\n"
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{name: "empty file", yaml: "", want: "config is empty"},
		{name: "unknown top-level field", yaml: "server:\n" + keys + "providers:\n  mock: {}\nextra: 1\n", want: "field extra not found"},
		{name: "unknown server field", yaml: "server:\n" + keys + "  port: 1\nproviders:\n  mock: {}\n", want: "field port not found"},
		{name: "no providers", yaml: "server:\n" + keys, want: "providers must configure at least one provider"},
		{name: "unknown provider", yaml: "server:\n" + keys + "providers:\n  nope: {}\n", want: `providers.nope: unknown provider`},
		{name: "field on mock", yaml: "server:\n" + keys + "providers:\n  mock: {api_key: x}\n", want: "field api_key not found"},
		{name: "bad listen", yaml: "server:\n  listen: nope\n" + keys + "providers:\n  mock: {}\n", want: `server.listen "nope"`},
		{name: "port out of range", yaml: "server:\n  listen: \"127.0.0.1:70000\"\n" + keys + "providers:\n  mock: {}\n", want: "server.listen"},
		{name: "port not a number", yaml: "server:\n  listen: \"127.0.0.1:http\"\n" + keys + "providers:\n  mock: {}\n", want: "server.listen"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Parse([]byte(tt.yaml))
			if err == nil {
				t.Fatal("Parse succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want substring %q", err, tt.want)
			}
		})
	}
}

func TestConfig_FmtNeverRevealsSecrets(t *testing.T) {
	t.Parallel()
	cfg, err := config.Parse([]byte("server:\n  api_keys: [sk-test-local-ci, {name: cursor, key: sk-test-local-cursor}]\nproviders:\n  mock: {}\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	values := map[string]any{"config": cfg, "server": cfg.Server, "local key": cfg.Server.APIKeys[1]}
	for name, v := range values {
		for _, verb := range []string{"%v", "%+v", "%#v"} {
			got := fmt.Sprintf(verb, v)
			for _, key := range []string{localKeyCI, localKeyCursor} {
				if strings.Contains(got, key) {
					t.Errorf("%s %s = %q, contains key", name, verb, got)
				}
			}
		}
	}
}

func TestLoad_ReadsFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "server:\n  api_keys: [sk-test-local-ci]\nproviders:\n  mock: {}\n")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.APIKeys[0].Name != "key-1" {
		t.Errorf("APIKeys[0].Name = %q, want key-1", cfg.Server.APIKeys[0].Name)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "missing.yaml")

	_, err := config.Load(path)

	if err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("error = %v, want it to name %s", err, path)
	}
}

func TestLoad_FileTooLarge(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "big.yaml")
	writeFile(t, path, "# "+strings.Repeat("x", 1<<20)+"\n")

	_, err := config.Load(path)

	if err == nil || !strings.Contains(err.Error(), "larger than 1 MiB") {
		t.Errorf("error = %v, want a size error", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
