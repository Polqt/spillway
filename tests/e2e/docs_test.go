package e2e_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
