package cli_test

import (
	"strings"
	"testing"

	"github.com/Polqt/spillway/internal/cli"
)

func TestRun_KeyPrintsRequestedLocalKeyWithoutProviderEnv(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, `server:
  api_keys:
    - name: default
      key: sk-test-local-default
    - name: dashboard
      key: sk-test-local-dashboard
      dashboard: true
providers:
  gemini:
    api_key: ${GEMINI_API_KEY}
`)
	for _, test := range []struct {
		name string
		want string
	}{
		{name: "default", want: "sk-test-local-default"},
		{name: "dashboard", want: "sk-test-local-dashboard"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr strings.Builder
			code := cli.Run(t.Context(), []string{"key", "-config", path, test.name}, nil, &stdout, &stderr, noEnv)
			if code != 0 || stdout.String() != test.want+"\n" || stderr.Len() != 0 {
				t.Fatalf("key = %d, stdout %q, stderr %q; want only requested key", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestRun_KeyMissingNameDoesNotPrintOtherKeys(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, "server:\n  api_keys:\n    - name: default\n      key: sk-test-local-key\n")
	var stdout, stderr strings.Builder
	code := cli.Run(t.Context(), []string{"key", "-config", path, "missing"}, nil, &stdout, &stderr, noEnv)
	if code == 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), `local key "missing" not found`) || strings.Contains(stderr.String(), "sk-test-local-key") {
		t.Fatalf("key = %d, stdout %q, stderr %q; want a safe missing-key error", code, stdout.String(), stderr.String())
	}
}

func TestRun_KeyExpandsLocalEnvironmentReference(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, "server:\n  api_keys:\n    - name: default\n      key: ${LOCAL_APP_KEY}\n")
	const key = "sk-test-local-env-key"
	lookup := func(name string) (string, bool) { return key, name == "LOCAL_APP_KEY" }
	var stdout, stderr strings.Builder
	code := cli.Run(t.Context(), []string{"key", "-config", path, "default"}, nil, &stdout, &stderr, lookup)
	if code != 0 || stdout.String() != key+"\n" || stderr.Len() != 0 {
		t.Fatalf("key = %d, stdout %q, stderr %q; want expanded local key", code, stdout.String(), stderr.String())
	}
}
