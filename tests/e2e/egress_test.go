package e2e_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestEgress_OnlyAllowlistedURLLiterals(t *testing.T) {
	root := filepath.Join("..", "..")
	got, err := productionURLLiterals(root)
	if err != nil {
		t.Fatal(err)
	}

	egressAllowlist := map[string]struct{}{
		"https://generativelanguage.googleapis.com/v1beta/openai": {},
		"https://api.groq.com/openai/v1":                          {},
		"https://openrouter.ai/api/v1":                            {},
		"http://localhost:11434":                                  {},
	}
	localEndpoints := map[string]struct{}{"http://127.0.0.1:8080": {}} // Local endpoint used by the eval command.
	printedOnlyURLs := map[string]struct{}{"https://github.com/Polqt/spillway/releases": {}}

	for value, source := range got {
		if _, ok := egressAllowlist[value]; ok {
			continue
		}
		if _, ok := printedOnlyURLs[value]; ok {
			if !strings.Contains(source, "internal/cli/install_channel.go") {
				t.Errorf("printed-only URL %q appears in unexpected production code %s", value, source)
			}
			continue
		}
		if _, ok := localEndpoints[value]; !ok {
			t.Errorf("unallowlisted URL literal %q in %s", value, source)
		}
	}
}

func TestEgress_ReleasesPageIsPrintedOnly(t *testing.T) {
	root := filepath.Join("..", "..")
	got, err := productionURLLiterals(root)
	if err != nil {
		t.Fatal(err)
	}
	source, exists := got["https://github.com/Polqt/spillway/releases"]
	if !exists {
		t.Fatal("release page URL is absent")
	}
	if !strings.Contains(source, "internal/cli/install_channel.go") {
		t.Errorf("source = %q", source)
	}
}

func productionURLLiterals(root string) (map[string]string, error) {
	urls := make(map[string]string)
	for _, dir := range []string{"internal", "apps"} {
		base := filepath.Join(root, dir)
		err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(node ast.Node) bool {
				literal, ok := node.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return true
				}
				value, err := strconv.Unquote(literal.Value)
				if err == nil && (strings.Contains(value, "http://") || strings.Contains(value, "https://")) {
					urls[value] = filepath.ToSlash(path)
				}
				return true
			})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return urls, nil
}
