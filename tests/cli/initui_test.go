package cli_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Polqt/spillway/internal/cli/initui"
)

func TestInitWizardSavesOnlyAfterReviewConfirmation(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var saved bool
	var output bytes.Buffer
	prepare := func(draft initui.Draft) (initui.Prepared, error) {
		return initui.Prepared{
			Preview: "api_key: \"${GEMINI_API_KEY}\"",
			Success: "Setup complete",
			Save: func(context.Context) error {
				saved = true
				return os.WriteFile(draft.ConfigPath, []byte("ok"), 0o600)
			},
		}, nil
	}
	keys := "\rj j\r\r\tj\rj\r\r\r"
	result, err := initui.Run(ctx, initui.Options{
		ConfigPath: path,
		Providers: []initui.Provider{
			{Name: "gemini", Hint: "key from GEMINI_API_KEY", SuggestedModel: "flash", FreeTierAttestable: true},
			{Name: "ollama", Hint: "local", SuggestedModel: "echo"},
		},
		Prepare: prepare,
	}, strings.NewReader(keys), &output)
	if err != nil {
		t.Fatalf("Run() error = %v; last screen: %q", err, output.String())
	}
	if !saved {
		t.Fatal("wizard returned without saving after review confirmation")
	}
	if result.Draft.ConfigPath != path || !strings.Contains(result.Success, "Setup complete") {
		t.Fatalf("unexpected result: %+v; want path %q", result, path)
	}
	if strings.Contains(output.String(), "sk-spillway-") {
		t.Fatal("wizard output included a generated local key")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config was not created: %v", err)
	}
}

func TestInitWizardCancellationDoesNotSave(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	saved := false
	_, err := initui.Run(context.Background(), initui.Options{
		ConfigPath: path,
		Providers:  []initui.Provider{{Name: "ollama", SuggestedModel: "echo"}},
		Prepare: func(initui.Draft) (initui.Prepared, error) {
			return initui.Prepared{Save: func(context.Context) error { saved = true; return nil }}, nil
		},
	}, strings.NewReader("\x1b"), &bytes.Buffer{})
	if err == nil || saved {
		t.Fatalf("Run() = %v, saved = %t; want cancellation without save", err, saved)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("config exists after cancel: %v", statErr)
	}
}

func TestInitWizardAddsNamedGeminiProject(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	var draft initui.Draft
	prepare := func(value initui.Draft) (initui.Prepared, error) {
		draft = value
		return initui.Prepared{
			Preview: "providers:\n  research:\n    api_key: \"${GEMINI_RESEARCH_API_KEY}\"",
			Success: "Setup complete",
			Save:    func(context.Context) error { return nil },
		}, nil
	}
	keys := "\r j\rresearch\rj\r j\rj\rj\rj\r\r\r"
	_, err := initui.Run(context.Background(), initui.Options{
		ConfigPath: path,
		Providers:  []initui.Provider{{Name: "gemini", SuggestedModel: "gemini-2.5-flash", FreeTierAttestable: true}},
		ValidateGeminiAlias: func(alias string) error {
			if alias != "research" {
				return fmt.Errorf("unexpected alias %q", alias)
			}
			return nil
		},
		Prepare: prepare,
	}, strings.NewReader(keys), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !slices.Contains(draft.GeminiAliases, "research") {
		t.Fatalf("Draft.GeminiAliases = %v; want research", draft.GeminiAliases)
	}
	if got := initui.GeminiAliasKeyEnv(draft.GeminiAliases[0]); got != "GEMINI_RESEARCH_API_KEY" {
		t.Errorf("GeminiAliasKeyEnv() = %q", got)
	}
}
