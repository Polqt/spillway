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

func openEditFixture(t *testing.T, name string) (*config.Document, []byte) {
	t.Helper()
	path := filepath.Join("testdata", "edit", name)
	data, err := os.ReadFile(path) // #nosec G304 -- fixtures are repository-owned testdata.
	if err != nil {
		t.Fatal(err)
	}
	doc, err := config.OpenDocument(path)
	if err != nil {
		t.Fatalf("OpenDocument(%s): %v", path, err)
	}
	return doc, data
}

func TestEdit_AddProviderToInitConfigInsertsOnlyTheProviderLines(t *testing.T) {
	t.Parallel()
	doc, before := openEditFixture(t, "init-groq.yaml")
	edit := doc.Edit()
	if err := edit.AddProvider(config.ProviderEntry{Name: "gemini", KeyEnv: "GEMINI_API_KEY", FreeTier: true}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	change, err := edit.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	want := string(before[:len("server:\n  listen: 127.0.0.1:8080\n  api_keys:\n    - name: default\n      key: \"sk-test-local-default-edit\"\nproviders:\n  groq:\n    api_key: \"${GROQ_API_KEY}\"\n    free_tier: true\n")]) +
		"  gemini:\n    api_key: \"${GEMINI_API_KEY}\"\n    free_tier: true\n" +
		string(before[len("server:\n  listen: 127.0.0.1:8080\n  api_keys:\n    - name: default\n      key: \"sk-test-local-default-edit\"\nproviders:\n  groq:\n    api_key: \"${GROQ_API_KEY}\"\n    free_tier: true\n"):])
	if got := string(change.Content()); got != want {
		t.Errorf("Content() =\n%s\nwant\n%s", got, want)
	}
}

func TestEdit_AddProviderKeepsEveryOtherByte(t *testing.T) {
	t.Parallel()
	paths, err := filepath.Glob(filepath.Join("testdata", "compat", "*", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	paths = append(paths, filepath.Join("testdata", "edit", "init-groq.yaml"))
	for _, path := range paths {
		data, err := os.ReadFile(path) // #nosec G304 -- fixtures are repository-owned testdata.
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := config.ParseWithEnv(data, placeholderEnv)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		entry, ok := unconfiguredProvider(cfg)
		if !ok {
			continue
		}
		doc, err := config.OpenDocument(path)
		if err != nil {
			t.Fatalf("OpenDocument(%s): %v", path, err)
		}
		edit := doc.Edit()
		if err := edit.AddProvider(entry); err != nil {
			t.Fatalf("%s: AddProvider(%s): %v", path, entry.Name, err)
		}
		change, err := edit.Result()
		if err != nil {
			t.Fatalf("%s: Result: %v", path, err)
		}
		inserted := insertedBlock(t, path, data, change.Content())
		if first := strings.TrimSpace(strings.SplitN(inserted, "\n", 2)[0]); !strings.HasPrefix(first, entry.Name+":") {
			t.Errorf("%s: inserted block starts with %q, want the %s entry", path, first, entry.Name)
		}
	}
}

func TestEdit_AddProviderPreservesCommentsBlankLinesAndQuotes(t *testing.T) {
	t.Parallel()
	doc, before := openEditFixture(t, "commented.yaml")
	edit := doc.Edit()
	if err := edit.AddProvider(config.ProviderEntry{Name: "ollama", BaseURL: "http://localhost:11434"}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	change, err := edit.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	anchor := "        free_tier: true   # attested 2026-10-19\n"
	at := strings.Index(string(before), anchor) + len(anchor)
	want := string(before[:at]) + "    ollama:\n        base_url: \"http://localhost:11434\"\n" + string(before[at:])
	if got := string(change.Content()); got != want {
		t.Errorf("Content() =\n%s\nwant\n%s", got, want)
	}
}

func TestEdit_PreservesCRLFAndBOM(t *testing.T) {
	t.Parallel()
	lf, err := os.ReadFile(filepath.Join("testdata", "edit", "init-groq.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	bom := []byte{0xef, 0xbb, 0xbf}
	before := append(append([]byte(nil), bom...), bytes.ReplaceAll(lf, []byte("\n"), []byte("\r\n"))...)
	path := filepath.Join(t.TempDir(), "config.yaml")
	// #nosec G703 -- this path is created under testing.T.TempDir.
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := config.OpenDocument(path)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	edit := doc.Edit()
	if err := edit.AddProvider(config.ProviderEntry{Name: "mock"}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	change, err := edit.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	anchor := "    free_tier: true\r\n"
	at := bytes.Index(before, []byte(anchor)) + len(anchor)
	want := string(before[:at]) + "  mock: {}\r\n" + string(before[at:])
	if got := string(change.Content()); got != want {
		t.Errorf("Content() = %q\nwant %q", got, want)
	}
}

func TestEdit_AppendTierModelBlockSequence(t *testing.T) {
	t.Parallel()
	doc, before := openEditFixture(t, "init-groq.yaml")
	edit := doc.Edit()
	if err := edit.AddProvider(config.ProviderEntry{Name: "gemini", KeyEnv: "GEMINI_API_KEY", FreeTier: true}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	if err := edit.AppendTierModel("free", "gemini/gemini-2.5-flash"); err != nil {
		t.Fatalf("AppendTierModel: %v", err)
	}
	change, err := edit.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	want := strings.Replace(string(before), "    free_tier: true\n", "    free_tier: true\n  gemini:\n    api_key: \"${GEMINI_API_KEY}\"\n    free_tier: true\n", 1)
	want = strings.Replace(want, "      - groq/llama-3.3-70b-versatile\n", "      - groq/llama-3.3-70b-versatile\n      - gemini/gemini-2.5-flash\n", 1)
	if got := string(change.Content()); got != want {
		t.Errorf("Content() =\n%s\nwant\n%s", got, want)
	}

	doc, _ = openEditFixture(t, "commented.yaml")
	edit = doc.Edit()
	if err := edit.AddProvider(config.ProviderEntry{Name: "mock"}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	if err := edit.AppendTierModel("free", "mock/echo"); err != nil {
		t.Fatalf("AppendTierModel: %v", err)
	}
	change, err = edit.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	item := "      - groq/llama-3.3-70b-versatile   # suggested\n"
	if got := string(change.Content()); !strings.Contains(got, item+"      - mock/echo\n      # - groq/other\n") {
		t.Errorf("Content() does not place mock/echo after the last item and before the trailing comment:\n%s", got)
	}
}

func TestEdit_FlowStyleTargetReturnsLayoutErrorWithSnippet(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, config, wantPath, wantSnippet string
		wantLine                            int
		edit                                func(*config.Edit) error
	}{
		{
			name:        "flow providers",
			config:      "server:\n  api_keys: [\"sk-test-local-flow-edit\"]\nproviders: {groq: {api_key: \"${GROQ_API_KEY}\"}}\n",
			wantPath:    "providers",
			wantLine:    3,
			wantSnippet: "providers:\n  gemini:\n    api_key: \"${GEMINI_API_KEY}\"\n",
			edit: func(e *config.Edit) error {
				return e.AddProvider(config.ProviderEntry{Name: "gemini", KeyEnv: "GEMINI_API_KEY"})
			},
		},
		{
			name:        "flow models",
			config:      "server:\n  api_keys: [\"sk-test-local-flow-edit\"]\nproviders:\n  mock: {}\ntiers:\n  free:\n    models: [mock/echo]\n",
			wantPath:    "tiers.free.models",
			wantLine:    7,
			wantSnippet: "tiers:\n  free:\n    models:\n      - mock/other\n",
			edit:        func(e *config.Edit) error { return e.AppendTierModel("free", "mock/other") },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			layout := editLayoutError(t, tt.config, tt.edit)
			if layout.Path != tt.wantPath || layout.Line != tt.wantLine || layout.Snippet != tt.wantSnippet {
				t.Errorf("LayoutError = %+v, want path %q line %d snippet %q", *layout, tt.wantPath, tt.wantLine, tt.wantSnippet)
			}
		})
	}
}

func TestEdit_AnchorsAliasesAndMultipleDocumentsRefused(t *testing.T) {
	t.Parallel()
	base := "server:\n  api_keys: [\"sk-test-local-anchor-edit\"]\nproviders:\n  mock: {}\ntiers:\n  free:\n    models:\n      - mock/echo\n"
	addOllama := func(e *config.Edit) error { return e.AddProvider(config.ProviderEntry{Name: "ollama"}) }
	for name, input := range map[string]string{
		"anchor":             strings.Replace(base, "    models:\n", "    models: &models\n", 1),
		"alias":              base + "router:\n  mode: rules\n  auto: [free]\n  difficulty:\n    simple: &simple [free]\n    moderate: *simple\n    hard: *simple\n",
		"multiple documents": base + "---\nserver: {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			layout := editLayoutError(t, input, addOllama)
			if layout.Path != "providers" || !strings.Contains(layout.Snippet, "ollama: {}") {
				t.Errorf("LayoutError = %+v, want a providers snippet for ollama", *layout)
			}
		})
	}
}

func TestEdit_ResultValidatesWithUniquePlaceholders(t *testing.T) {
	t.Parallel()
	// Two ${VAR} local keys would look like duplicates if every variable expanded to the same value.
	input := "server:\n  api_keys:\n    - name: ci\n      key: \"${CI_KEY}\"\n    - name: cursor\n      key: \"${CURSOR_KEY}\"\nproviders:\n  mock: {}\n"
	doc := writeDocument(t, input)
	edit := doc.Edit()
	if err := edit.AddProvider(config.ProviderEntry{Name: "ollama"}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	if _, err := edit.Result(); err != nil {
		t.Fatalf("Result: %v", err)
	}
}

func TestEdit_InvalidResultRejectedFileUntouched(t *testing.T) {
	t.Parallel()
	path := filepath.Join("testdata", "edit", "init-groq.yaml")
	before, err := os.ReadFile(path) // #nosec G304 -- fixture is repository-owned testdata.
	if err != nil {
		t.Fatal(err)
	}
	doc, err := config.OpenDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	edit := doc.Edit()
	// openrouter is not configured, so the tier would reference an unknown provider.
	if err := edit.AppendTierModel("free", "openrouter/meta-llama/llama-3.3-70b-instruct:free"); err != nil {
		t.Fatalf("AppendTierModel: %v", err)
	}
	_, err = edit.Result()
	if err == nil || !strings.Contains(err.Error(), "edited config would be invalid") {
		t.Fatalf("Result error = %v, want an invalid-result error", err)
	}
	after, err := os.ReadFile(path) // #nosec G304 -- fixture is repository-owned testdata.
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("Result changed the file on disk")
	}
}

func TestEdit_AddProviderRefusesConfiguredProvider(t *testing.T) {
	t.Parallel()
	doc, _ := openEditFixture(t, "init-groq.yaml")
	err := doc.Edit().AddProvider(config.ProviderEntry{Name: "groq", KeyEnv: "GROQ_API_KEY_2"})
	var exists *config.ProviderExistsError
	if !errors.As(err, &exists) || exists.Name != "groq" || exists.Line != 7 {
		t.Fatalf("error = %v, want ProviderExistsError for groq at line 7", err)
	}
}

func TestEdit_ProviderEntryRejectsNonVariableKeyEnvWithoutEcho(t *testing.T) {
	t.Parallel()
	doc, _ := openEditFixture(t, "init-groq.yaml")
	for _, value := range []string{"sk-test-canary-key-value", "AIzaSyCanaryCanaryCanary", "gsk_CanaryCanaryCanary", strings.Repeat("K", 65), "${GEMINI_API_KEY}", "GEMINI API KEY"} {
		err := doc.Edit().AddProvider(config.ProviderEntry{Name: "gemini", KeyEnv: value})
		if err == nil {
			t.Errorf("AddProvider(KeyEnv %q) succeeded, want refusal", value)
			continue
		}
		if strings.Contains(err.Error(), value) {
			t.Errorf("error %q echoes the rejected value", err)
		}
	}
}

func TestEdit_PreviewRedactsLocalKeys(t *testing.T) {
	t.Parallel()
	const localKey = "sk-test-local-preview-canary"
	doc := writeDocument(t, "server:\n  api_keys:\n    - name: default\n      key: \""+localKey+"\"\nproviders:\n  mock: {}\n")
	edit := doc.Edit()
	if err := edit.AddProvider(config.ProviderEntry{Name: "gemini", KeyEnv: "GEMINI_API_KEY"}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	change, err := edit.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	var preview strings.Builder
	if err := change.WritePreview(&preview); err != nil {
		t.Fatal(err)
	}
	want := "@@ providers @@\n" +
		"       key: \"[REDACTED]\"\n" +
		" providers:\n" +
		"   mock: {}\n" +
		"+  gemini:\n" +
		"+    api_key: \"${GEMINI_API_KEY}\"\n"
	if preview.String() != want {
		t.Errorf("preview =\n%s\nwant\n%s", preview.String(), want)
	}
}

func writeDocument(t *testing.T, input string) *config.Document {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := config.OpenDocument(path)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	return doc
}

// editLayoutError writes input, applies edit and requires a LayoutError.
func editLayoutError(t *testing.T, input string, edit func(*config.Edit) error) *config.LayoutError {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := config.OpenDocument(path)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	e := doc.Edit()
	err = edit(e)
	if err == nil {
		_, err = e.Result()
	}
	var layout *config.LayoutError
	if !errors.As(err, &layout) {
		t.Fatalf("error = %v, want *config.LayoutError", err)
	}
	return layout
}

func placeholderEnv(name string) (string, bool) {
	return "sk-test-" + strings.ToLower(name), true
}

// unconfiguredProvider picks the first preset the config does not use yet.
func unconfiguredProvider(cfg config.Config) (config.ProviderEntry, bool) {
	candidates := []config.ProviderEntry{
		{Name: "gemini", KeyEnv: "GEMINI_API_KEY"},
		{Name: "groq", KeyEnv: "GROQ_API_KEY"},
		{Name: "openrouter", KeyEnv: "OPENROUTER_API_KEY"},
		{Name: "ollama"},
		{Name: "mock"},
	}
	for _, entry := range candidates {
		if _, ok := cfg.Providers[entry.Name]; !ok {
			return entry, true
		}
	}
	return config.ProviderEntry{}, false
}

// insertedBlock requires after to be before plus one contiguous block of whole
// lines, and returns that block.
func insertedBlock(t *testing.T, name string, before, after []byte) string {
	t.Helper()
	prefix := 0
	for prefix < len(before) && prefix < len(after) && before[prefix] == after[prefix] {
		prefix++
	}
	// Back up to a line start so the block is whole lines.
	for prefix > 0 && before[prefix-1] != '\n' {
		prefix--
	}
	if len(after) < len(before) || !bytes.HasSuffix(after, before[prefix:]) {
		t.Fatalf("%s: edit changed bytes outside one inserted block:\n%s", name, after)
	}
	return string(after[prefix : len(after)-(len(before)-prefix)])
}
