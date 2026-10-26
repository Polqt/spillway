package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Polqt/spillway/internal/cli"
)

func TestSetupClientsPreserveSettingsAndAvoidPrintingLocalKey(t *testing.T) {
	t.Parallel()
	for _, client := range []string{"opencode", "claude", "codex"} {
		t.Run(client, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			const localKey = "sk-test-setup-local-key-secret"
			configText := "server:\n  listen: 127.0.0.1:9099\n  api_keys:\n    - name: default\n      key: " + localKey + "\nproviders: {}\n"
			if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			lookup := func(name string) (string, bool) {
				switch name {
				case "USERPROFILE", "HOME":
					return home, true
				case "CODEX_HOME":
					return filepath.Join(home, ".codex"), true
				default:
					return "", false
				}
			}
			options := cli.Options{LookupEnv: lookup, Executable: func() (string, error) { return filepath.Join(t.TempDir(), "spillway.exe"), nil }, Now: func() time.Time { return time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC) }}
			if client == "opencode" {
				path := filepath.Join(home, ".config", "opencode", "opencode.json")
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(`{"theme":"dark","provider":{"other":{"name":"Keep"}}}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if code := cli.RunWithOptions(context.Background(), []string{"setup", client, "-config", configPath}, nil, &stdout, &stderr, options); code != 0 {
				t.Fatalf("setup exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if strings.Contains(stdout.String()+stderr.String(), localKey) {
				t.Fatal("setup printed the local key")
			}
			next := map[string]string{"opencode": "Next: opencode", "claude": "Next: claude", "codex": "Next: codex --profile spillway"}[client]
			if !strings.Contains(stdout.String(), next) {
				t.Fatalf("setup output does not show next command %q: %s", next, stdout.String())
			}
			if code := cli.RunWithOptions(context.Background(), []string{"setup", client, "-config", configPath}, nil, &stdout, &stderr, options); code != 0 {
				t.Fatalf("repeat setup exit=%d stderr=%q", code, stderr.String())
			}
			switch client {
			case "opencode":
				path := filepath.Join(home, ".config", "opencode", "opencode.json")
				data, err := os.ReadFile(path) // #nosec G304 -- path is a fixed global OpenCode config under t.TempDir.
				if err != nil {
					t.Fatal(err)
				}
				var got map[string]any
				if err := json.Unmarshal(data, &got); err != nil {
					t.Fatal(err)
				}
				if got["theme"] != "dark" || got["model"] != "spillway/auto" || strings.Contains(string(data), localKey) || !strings.Contains(string(data), "{file:./spillway.key}") || !strings.Contains(string(data), "127.0.0.1:9099/v1") {
					t.Fatalf("OpenCode settings missing preserved or Spillway config: %s", data)
				}
				keyData, err := os.ReadFile(filepath.Join(filepath.Dir(path), "spillway.key"))
				if err != nil || string(keyData) != localKey {
					t.Fatalf("OpenCode key file = %q, err=%v", keyData, err)
				}
			case "claude":
				path := filepath.Join(home, ".claude", "settings.json")
				data, err := os.ReadFile(path) // #nosec G304 -- path is the global client config under t.TempDir.
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(data), localKey) || !strings.Contains(string(data), "apiKeyHelper") || !strings.Contains(string(data), "ANTHROPIC_BASE_URL") {
					t.Fatalf("Claude settings missing helper or contains key: %s", data)
				}
			case "codex":
				path := filepath.Join(home, ".codex", "spillway.config.toml")
				data, err := os.ReadFile(path) // #nosec G304 -- path is the global client config under t.TempDir.
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(data), localKey) || !strings.Contains(string(data), "wire_api = \"responses\"") || !strings.Contains(string(data), "[model_providers.spillway.auth]") || !strings.Contains(string(data), "args = [\"key\", \"-config\", ") {
					t.Fatalf("Codex profile missing Responses provider or contains key: %s", data)
				}
			}
		})
	}
}

func TestSetupRejectsNonLoopbackServerAddress(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("server:\n  listen: 192.0.2.10:8080\n  api_keys:\n    - name: default\n      key: sk-test-local\nproviders: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	options := cli.Options{LookupEnv: func(name string) (string, bool) {
		if name == "USERPROFILE" {
			return home, true
		}
		return "", false
	}}
	if code := cli.RunWithOptions(context.Background(), []string{"setup", "claude", "-config", configPath}, nil, &stdout, &stderr, options); code != 1 || !strings.Contains(stderr.String(), "loopback") {
		t.Fatalf("setup exit=%d stderr=%q", code, stderr.String())
	}
}
