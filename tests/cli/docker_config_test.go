package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Polqt/spillway/internal/config"
)

func TestExamples_DockerConfigValidates(t *testing.T) {
	t.Parallel()
	example := filepath.Join("..", "..", "examples", "config.docker.yaml")
	// #nosec G304 -- this path is fixed within the repository.
	contents, err := os.ReadFile(example)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(contents), "path: /var/lib/spillway/spillway.db") {
		t.Fatal("Docker example store path must be under /var/lib/spillway")
	}
	storeDir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	contents = []byte(strings.ReplaceAll(string(contents), "/var/lib/spillway/spillway.db", filepath.Join(storeDir, "spillway.db")))
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	// #nosec G703 -- this path is created under testing.T.TempDir.
	if err := os.WriteFile(configPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Listen != "0.0.0.0:8080" {
		t.Errorf("Listen = %q, want 0.0.0.0:8080", cfg.Server.Listen)
	}
	if len(cfg.Server.APIKeys) != 1 || !strings.HasPrefix(cfg.Server.APIKeys[0].Key.Reveal(), "sk-test-") {
		t.Errorf("api_keys = %v, want one fake local key", cfg.Server.APIKeys)
	}
	if _, ok := cfg.Providers["ollama"]; !ok || len(cfg.Providers) != 1 {
		t.Errorf("providers = %v, want only ollama", cfg.Providers)
	}
	if cfg.Store == nil || filepath.Dir(cfg.Store.Path) != storeDir {
		t.Errorf("store = %+v, want a file under %q", cfg.Store, storeDir)
	}
}
