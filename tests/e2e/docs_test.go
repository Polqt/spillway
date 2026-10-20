package e2e_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Polqt/spillway/internal/cli"
	"github.com/Polqt/spillway/internal/config"
)

func TestDocs_ClientExamplesUseLocalEndpointAutoAndPlaceholder(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	guide, err := os.ReadFile(filepath.Join(root, "docs", "clients.md")) // #nosec G304 -- root is the repository root found from this test's source directory.
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(guide), "OPENAI_BASE_URL=http://127.0.0.1:8080/v1") || !strings.Contains(string(guide), "OPENAI_API_KEY=<LOCAL_APP_KEY>") || !strings.Contains(string(guide), "default") {
		t.Fatalf("client guide lacks endpoint, key variables or default key guidance")
	}
	files := []string{"curl.sh", "curl.ps1", "python.py", "node.mjs", "env.sh"}
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(root, "examples", "clients", name)) // #nosec G304 -- name is drawn from a fixed test list.
		if err != nil {
			t.Fatalf("read client example %s: %v", name, err)
		}
		content := string(data)
		for _, want := range []string{"127.0.0.1:8080/v1", "auto", "<LOCAL_APP_KEY>"} {
			if !strings.Contains(content, want) {
				t.Errorf("%s missing %q", name, want)
			}
		}
		if strings.Contains(content, "sk-spillway-") {
			t.Errorf("%s contains a generated local-key literal", name)
		}
	}
}

func TestDocs_CoreCommandExamplesParse(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	docPaths := []string{"README.md", filepath.Join("docs", "install.md"), filepath.Join("docs", "configuration.md"), filepath.Join("docs", "troubleshooting.md")}
	var documentation strings.Builder
	for _, relative := range docPaths {
		data, err := os.ReadFile(filepath.Join(root, relative)) // #nosec G304 -- paths are fixed documentation files under the repository root.
		if err != nil {
			t.Fatal(err)
		}
		documentation.Write(data)
	}
	for _, command := range []string{"spillway doctor", "spillway provider list", "spillway provider show", "spillway provider add"} {
		if !strings.Contains(documentation.String(), command) {
			t.Errorf("core command %q is not documented", command)
		}
	}
	commands := [][]string{
		{"doctor", "-config", "config.yaml"},
		{"doctor", "--no-probe", "-config", "config.yaml"},
		{"provider", "list", "-config", "config.yaml"},
		{"provider", "show", "groq", "-config", "config.yaml"},
		{"provider", "add", "mock", "--tier", "free", "--yes", "-config", "config.yaml"},
	}
	for _, args := range commands {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr strings.Builder
			args = append(append([]string(nil), args...), "--help")
			if code := cli.Run(t.Context(), args, nil, &stdout, &stderr, noEnvironment); code != 0 {
				t.Fatalf("documented command help exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestDocs_ClientsStateSupportedProtocols(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	guide, err := os.ReadFile(filepath.Join(root, "docs", "clients.md")) // #nosec G304 -- fixed document path under repository root.
	if err != nil {
		t.Fatal(err)
	}
	content := string(guide)
	for _, want := range []string{"/v1/chat/completions", "/v1/models", "/v1/responses", "does not implement the OpenAI Responses API"} {
		if !strings.Contains(content, want) {
			t.Errorf("client guide lacks %q", want)
		}
	}
}

func noEnvironment(string) (string, bool) { return "", false }

func TestDocs_FullConfigExamplesValidate(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(root, "README.md")}
	err = filepath.WalkDir(filepath.Join(root, "docs"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && filepath.Ext(path) == ".md" {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	checked := 0
	for _, path := range paths {
		data, err := os.ReadFile(path) // #nosec G304 -- paths are README.md or Markdown files walked under the repository docs directory.
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		examples, err := fullConfigExamples(string(data))
		if err != nil {
			t.Errorf("%s: %v", filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator))), err)
			continue
		}
		for blockNumber, example := range examples {
			checked++
			if _, err := config.ParseWithEnv([]byte(example), func(string) (string, bool) {
				return "sk-test-docs", true
			}); err != nil {
				t.Errorf("%s config example %d: %v", filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator))), blockNumber+1, err)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no fenced yaml spillway-config examples found in README.md or docs/")
	}
}

func TestDocs_TroubleshootingCoversCLIRecoveryCases(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	guide, err := os.ReadFile(filepath.Join(root, "docs", "troubleshooting.md")) // #nosec G304 -- path is fixed under the repository root.
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		want string
	}{
		{name: "init does not overwrite configs", want: "init never overwrites a config"},
		{name: "missing environment variable", want: "environment variable"},
		{name: "set a variable in the current shell", want: "set it in this shell"},
		{name: "address already in use", want: "Another process (possibly an older Spillway)"},
		{name: "newer store schema", want: "newer Spillway binary that wrote this database"},
		{name: "start after config check", want: "spillway serve -config"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(string(guide), tt.want) {
				t.Errorf("troubleshooting guide does not cover %q", tt.want)
			}
		})
	}
}

func fullConfigExamples(markdown string) ([]string, error) {
	const info = "yaml spillway-config"
	var examples []string
	var current []string
	inExample := false
	for _, line := range strings.Split(markdown, "\n") {
		trimmed := strings.TrimSpace(line)
		if !inExample {
			if trimmed == "```"+info {
				inExample = true
				current = nil
			}
			continue
		}
		if trimmed == "```" {
			examples = append(examples, strings.Join(current, "\n"))
			inExample = false
			continue
		}
		current = append(current, line)
	}
	if inExample {
		return nil, fmt.Errorf("unterminated fenced %s example", info)
	}
	return examples, nil
}
