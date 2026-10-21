package cli_test

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Polqt/spillway/internal/cli"
	"github.com/Polqt/spillway/internal/store"
	"time"
)

func TestDoctor_HealthyConfigExits0(t *testing.T) {
	t.Parallel()
	path := writeDoctorConfig(t, "gsk_test-provider-key-long-enough-for-shape-check", "sk-test-local-doctor-key-0123456789ABCDEF", "")
	var stdout, stderr strings.Builder
	options := cli.Options{LookupEnv: func(name string) (string, bool) {
		if name == "GROQ_API_KEY" {
			return "gsk_test-provider-key-long-enough-for-shape-check", true
		}
		return "", false
	}}
	code := cli.RunWithOptions(t.Context(), []string{"doctor", "-config", path}, nil, &stdout, &stderr, options)
	if code != 0 {
		t.Fatalf("exit = %d; stdout = %q; stderr = %q", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"Config", "valid with placeholder values", "providers in this config: groq", "Provider keys", "Local keys", "Install", "Responses API is not served"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout %q lacks %q", stdout.String(), want)
		}
	}
}

func TestDoctor_AnyFailExits1(t *testing.T) {
	t.Parallel()
	path := writeDoctorConfig(t, "", "sk-test-local-doctor-key-0123456789ABCDEF", "")
	var stdout, stderr strings.Builder
	code := cli.RunWithOptions(t.Context(), []string{"doctor", "-config", path}, nil, &stdout, &stderr, cli.Options{LookupEnv: noEnv})
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stdout = %q", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "GROQ_API_KEY") || !strings.Contains(stdout.String(), "not set") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestDoctor_PrintsEffectiveAbsoluteConfigPathAndProviders(t *testing.T) {
	t.Parallel()
	path := writeDoctorConfig(t, "gsk_test-provider-key-long-enough-for-shape-check", "sk-test-local-doctor-key-0123456789ABCDEF", "")
	var stdout, stderr strings.Builder
	lookup := func(name string) (string, bool) {
		return "gsk_test-provider-key-long-enough-for-shape-check", name == "GROQ_API_KEY"
	}
	code := cli.RunWithOptions(t.Context(), []string{"doctor", "-config", path}, nil, &stdout, &stderr, cli.Options{LookupEnv: lookup})
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit = %d; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), absolute) || !strings.Contains(stdout.String(), "providers in this config: groq") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestDoctor_UnsetVariablePersistedForUserSaysOpenNewTerminal(t *testing.T) {
	t.Parallel()
	path := writeDoctorConfig(t, "", "sk-test-local-doctor-key-0123456789ABCDEF", "")
	var stdout, stderr strings.Builder
	options := cli.Options{GOOS: "windows", Shell: "powershell.exe", LookupEnv: noEnv, PersistentEnv: func(name string) (string, bool) {
		return "user", name == "GROQ_API_KEY"
	}}
	code := cli.RunWithOptions(t.Context(), []string{"doctor", "-config", path}, nil, &stdout, &stderr, options)
	if code != 1 {
		t.Fatalf("exit = %d; stderr = %q", code, stderr.String())
	}
	for _, want := range []string{"set for your Windows user", "open a new terminal", "Read-Host -AsSecureString", "SetEnvironmentVariable"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout %q lacks %q", stdout.String(), want)
		}
	}
}

func TestDoctor_UnsetVariableNotPersistedExplainsSessionOnlyVariable(t *testing.T) {
	t.Parallel()
	path := writeDoctorConfig(t, "", "sk-test-local-doctor-key-0123456789ABCDEF", "")
	var stdout, stderr strings.Builder
	options := cli.Options{GOOS: "linux", Shell: "/bin/bash", LookupEnv: noEnv}
	code := cli.RunWithOptions(t.Context(), []string{"doctor", "-config", path}, nil, &stdout, &stderr, options)
	if code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout.String(), "not set in this terminal or persistently") || !strings.Contains(stdout.String(), "read -rs GROQ_API_KEY && export GROQ_API_KEY") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestDoctor_ShellGuidancePerGOOSAndShell(t *testing.T) {
	t.Parallel()
	cases := []struct{ goos, shell, want string }{
		{"windows", "powershell.exe", "Read-Host -AsSecureString"},
		{"linux", "/bin/bash", "read -rs GROQ_API_KEY"},
		{"linux", "/bin/zsh", "read -rs GROQ_API_KEY"},
		{"darwin", "/bin/zsh", "read -rs GROQ_API_KEY"},
		{"linux", "/usr/bin/fish", "set -Ux GROQ_API_KEY $GROQ_API_KEY"},
		{"unknown", "unknown-shell", "read -rs GROQ_API_KEY"},
	}
	for _, test := range cases {
		t.Run(test.goos+"/"+filepath.Base(test.shell), func(t *testing.T) {
			path := writeDoctorConfig(t, "", "sk-test-local-doctor-key-0123456789ABCDEF", "")
			var stdout, stderr strings.Builder
			options := cli.Options{GOOS: test.goos, Shell: test.shell, LookupEnv: noEnv}
			code := cli.RunWithOptions(t.Context(), []string{"doctor", "-config", path}, nil, &stdout, &stderr, options)
			if code != 1 {
				t.Fatalf("exit = %d", code)
			}
			if !strings.Contains(stdout.String(), test.want) {
				t.Errorf("stdout = %q", stdout.String())
			}
			if strings.Contains(stdout.String(), "<provider-key>") {
				t.Error("guidance contains a key placeholder")
			}
		})
	}
}

func TestDoctor_ProviderKeyCertainMistakesFail(t *testing.T) {
	t.Parallel()
	for _, value := range []string{`"gsk_test-key-canary"`, " gsk_test-key-canary", "gsk_test-key-canary ", "Bearer gsk_test-key-canary", "${GROQ_API_KEY}", "<your-provider-key>"} {
		t.Run(fmt.Sprintf("case-%d", len(value)), func(t *testing.T) {
			path := writeDoctorConfig(t, value, "sk-test-local-doctor-key-0123456789ABCDEF", "")
			var stdout, stderr strings.Builder
			options := cli.Options{LookupEnv: func(name string) (string, bool) { return value, name == "GROQ_API_KEY" }}
			code := cli.RunWithOptions(t.Context(), []string{"doctor", "-config", path}, nil, &stdout, &stderr, options)
			if code != 1 {
				t.Fatalf("exit = %d, want 1", code)
			}
			if strings.Contains(stdout.String()+stderr.String(), value) {
				t.Fatalf("key value disclosed: %q", value)
			}
		})
	}
}

func TestDoctor_ProviderKeyFormatHintPrintsNoCharacters(t *testing.T) {
	t.Parallel()
	const canary = "AQ.7QZ-9XW-4PL-8JK-6RM"
	path := writeDoctorConfig(t, canary, "sk-test-local-doctor-key-0123456789ABCDEF", "")
	var stdout, stderr strings.Builder
	options := cli.Options{LookupEnv: func(name string) (string, bool) { return canary, name == "GROQ_API_KEY" }, Executable: func() (string, error) { return `C:\spillway.exe`, nil }}
	code := cli.RunWithOptions(t.Context(), []string{"doctor", "-config", path}, nil, &stdout, &stderr, options)
	if code != 0 {
		t.Fatalf("format-only issue exit = %d; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "usual public prefix") {
		t.Errorf("stdout = %q", stdout.String())
	}
	for start := 0; start+3 <= len(canary); start++ {
		fragment := canary[start : start+3]
		if strings.Contains(stdout.String()+stderr.String(), fragment) {
			t.Errorf("output contains a provider key fragment %q", fragment)
		}
	}
}

func TestDoctor_LocalKeysShowNoValuePrefixOrHash(t *testing.T) {
	t.Parallel()
	const secret = "sk-test-local-secret-canary-0123456789abcdef" //nolint:gosec // Deliberate fake credential canary used to assert output redaction.
	path := writeDoctorConfig(t, "gsk_test-provider-key-long-enough-for-shape-check", secret, "")
	var stdout, stderr strings.Builder
	lookup := func(name string) (string, bool) {
		return "gsk_test-provider-key-long-enough-for-shape-check", name == "GROQ_API_KEY"
	}
	code := cli.RunWithOptions(t.Context(), []string{"doctor", "-config", path}, nil, &stdout, &stderr, cli.Options{LookupEnv: lookup})
	if code != 0 {
		t.Fatalf("exit = %d; stderr = %q", code, stderr.String())
	}
	hash := sha256.Sum256([]byte(secret))
	for _, fragment := range []string{secret, secret[:8], secret[len(secret)-8:], hex.EncodeToString(hash[:])} {
		if strings.Contains(stdout.String()+stderr.String(), fragment) {
			t.Errorf("output contains local key data %q", fragment)
		}
	}
}

func TestDoctor_InstallChannelInstructions(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, goos, path, version, module, modified, assets, want string }{
		{"archive", "linux", "/usr/local/bin/spillway", "v1.1.0", "(devel)", "false", "embedded", "release archive"},
		{"go install", "linux", "/home/me/go/bin/spillway", "dev", "v1.1.0", "", "fallback", "go install"},
		{"docker", "linux", "/spillway", "dev", "(devel)", "", "fallback", "Docker image"},
		{"scoop", "windows", `C:\Users\me\scoop\apps\spillway\current\spillway.exe`, "dev", "(devel)", "", "fallback", "scoop"},
		{"homebrew", "darwin", "/opt/homebrew/bin/spillway", "dev", "(devel)", "", "fallback", "Homebrew"},
		{"local", "linux", "/workspace/bin/spillway", "dev", "(devel)", "", "fallback", "local or unknown build"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			path := writeDoctorConfig(t, "gsk_test-provider-key-long-enough-for-shape-check", "sk-test-local-doctor-key-0123456789ABCDEF", "")
			var stdout, stderr strings.Builder
			options := cli.Options{GOOS: test.goos, Version: test.version, ModuleVersion: test.module, VCSModified: test.modified, DashboardAssets: test.assets, LookupEnv: func(name string) (string, bool) {
				if name == "GROQ_API_KEY" {
					return "gsk_test-provider-key-long-enough-for-shape-check", true
				}
				return "", false
			}, Executable: func() (string, error) { return test.path, nil }}
			if code := cli.RunWithOptions(t.Context(), []string{"doctor", "-config", path}, nil, &stdout, &stderr, options); code != 0 {
				t.Fatalf("exit = %d; stderr = %q", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), test.want) {
				t.Errorf("stdout = %q", stdout.String())
			}
			if test.name == "go install" {
				if !strings.Contains(stdout.String(), "go install ./apps/spillway") {
					t.Errorf("doctor does not explain the supported source-checkout install: %q", stdout.String())
				}
				if strings.Contains(stdout.String(), "apps/spillway@v") {
					t.Errorf("doctor suggests the unsupported version-suffixed package install: %q", stdout.String())
				}
			}
		})
	}
}

func TestDoctor_PathShadowingWarnsWithoutRunningOtherBinary(t *testing.T) {
	t.Parallel()
	path := writeDoctorConfig(t, "gsk_test-provider-key-long-enough-for-shape-check", "sk-test-local-doctor-key-0123456789ABCDEF", "")
	binDir := t.TempDir()
	other := filepath.Join(binDir, "spillway.exe")
	marker := filepath.Join(t.TempDir(), "executed")
	if err := os.WriteFile(other, []byte("would create marker: "+marker), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	options := cli.Options{GOOS: "windows", LookupEnv: func(name string) (string, bool) {
		if name == "GROQ_API_KEY" {
			return "gsk_test-provider-key-long-enough-for-shape-check", true
		}
		if name == "PATH" {
			return binDir, true
		}
		return "", false
	}, Executable: func() (string, error) { return `C:\spillway.exe`, nil }}
	code := cli.RunWithOptions(t.Context(), []string{"doctor", "-config", path}, nil, &stdout, &stderr, options)
	if code != 0 {
		t.Fatalf("exit = %d; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "spillway on PATH resolves to") {
		t.Errorf("stdout = %q", stdout.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("PATH executable was run; marker stat error = %v", err)
	}
}

func TestDoctor_MakesNoProviderCalls(t *testing.T) {
	t.Parallel()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	path := writeDoctorConfig(t, "gsk_test-provider-key-long-enough-for-shape-check", "sk-test-local-doctor-key-0123456789ABCDEF", server.URL)
	var stdout, stderr strings.Builder
	lookup := func(name string) (string, bool) {
		return "gsk_test-provider-key-long-enough-for-shape-check", name == "GROQ_API_KEY"
	}
	if code := cli.RunWithOptions(t.Context(), []string{"doctor", "-config", path}, nil, &stdout, &stderr, cli.Options{LookupEnv: lookup}); code != 0 {
		t.Fatalf("exit = %d; stderr = %q", code, stderr.String())
	}
	if requests != 0 {
		t.Fatalf("provider requests = %d, want 0", requests)
	}
}

func TestDoctor_NewerStoreSchemaNeverSuggestsDeletion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	databasePath := filepath.Join(dir, "usage.db")
	opened, err := store.Open(t.Context(), store.Options{Path: databasePath, Retention: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	path := writeDoctorConfig(t, "gsk_test-provider-key-long-enough-for-shape-check", "sk-test-local-doctor-key-0123456789ABCDEF", "")
	// #nosec G304 -- path is returned by writeDoctorConfig and remains inside t.TempDir.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte("store:\n  path: \""+filepath.ToSlash(databasePath)+"\"\n  retention: 30d\n")...)
	// #nosec G703 -- path is constructed under t.TempDir in writeDoctorConfig.
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	lookup := func(name string) (string, bool) {
		return "gsk_test-provider-key-long-enough-for-shape-check", name == "GROQ_API_KEY"
	}
	code := cli.RunWithOptions(t.Context(), []string{"doctor", "-config", path}, nil, &stdout, &stderr, cli.Options{LookupEnv: lookup})
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr = %q", code, stderr.String())
	}
	for _, want := range []string{"newer than supported schema", "keep the existing database", "do not delete it"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout %q lacks %q", stdout.String(), want)
		}
	}
}

func writeDoctorConfig(t *testing.T, providerKey, localKey, baseURL string) string {
	t.Helper()
	providerEntry := `    api_key: "${GROQ_API_KEY}"` + "\n"
	if baseURL != "" {
		providerEntry += `    base_url: "` + baseURL + `"` + "\n"
	}
	content := "server:\n  listen: 127.0.0.1:0\n  api_keys:\n    - name: default\n      key: \"" + localKey + "\"\nlogging:\n  level: info\nproviders:\n  groq:\n" + providerEntry + "    free_tier: true\ntiers:\n  free:\n    models:\n      - groq/llama-3.3-70b-versatile\nrouter:\n  auto:\n    - free\n"
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = providerKey
	return path
}
