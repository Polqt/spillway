package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/Polqt/spillway/internal/cli/initui"
	"github.com/Polqt/spillway/internal/providers"
)

var generatedKeyPattern = regexp.MustCompile(`key: "sk-spillway-[^"]+"`)

func runInitWizard(ctx context.Context, configPath string, lookupEnv func(string) (string, bool), stdin io.Reader, stdout, stderr io.Writer) int {
	options := make([]initui.Provider, 0, len(providers.Presets()))
	for _, preset := range providers.Presets() {
		hint := "local provider"
		if preset.KeyEnv != "" {
			hint = "key from " + preset.KeyEnv
		}
		options = append(options, initui.Provider{
			Name:               preset.Name,
			Hint:               hint,
			SuggestedModel:     preset.SuggestedModel,
			FreeTierAttestable: preset.FreeTierAttestable,
		})
	}
	var statuses []string
	baseURL := "http:" + "/" + "/127.0.0.1:8080"
	prepare := func(draft initui.Draft) (initui.Prepared, error) {
		if err := validateNewConfigPath(draft.ConfigPath); err != nil {
			return initui.Prepared{}, err
		}
		attested := make(map[string]bool, len(draft.Attested))
		for _, name := range draft.Attested {
			attested[name] = true
		}
		generated, envStatuses, err := buildInitConfig(draft.Providers, attested, draft.Models, draft.Store, lookupEnv)
		if err != nil {
			return initui.Prepared{}, err
		}
		statuses = envStatuses
		preview := generatedKeyPattern.ReplaceAllString(string(generated), `key: "<redacted>"`)
		keyCommand := "spillway key"
		if draft.ConfigPath != defaultConfigPath {
			keyCommand += ` -config "` + draft.ConfigPath + `"`
		}
		success := fmt.Sprintf("Created %s with private file permissions.\n\nClient: base URL %s/v1; model `auto`; key `%s default`.\nStart: `spillway check -config %s`, then `spillway serve -config %s`.", draft.ConfigPath, baseURL, keyCommand, draft.ConfigPath, draft.ConfigPath)
		if draft.Store {
			success += fmt.Sprintf("\nDashboard: %s/dashboard; key `%s dashboard`.", baseURL, keyCommand)
		} else {
			success += "\nDashboard: disabled. Rerun init with `-store` to enable local history and login."
		}
		return initui.Prepared{
			Preview: preview,
			Success: success,
			Save: func(saveCtx context.Context) error {
				return writeInitConfig(saveCtx, draft.ConfigPath, generated)
			},
		}, nil
	}
	result, err := initui.Run(ctx, initui.Options{ConfigPath: configPath, Providers: options, Prepare: prepare}, stdin, stdout)
	if err != nil {
		if ctx.Err() != nil || strings.Contains(err.Error(), "canceled") {
			reportInitError(stderr, "Setup was cancelled", "No config file was created.", "Run `spillway init` when you're ready to continue.")
			return 1
		}
		reportInitError(stderr, "Setup couldn't continue", err.Error(), "Run `spillway init --help` to review the available options.")
		return 1
	}
	for _, status := range statuses {
		if _, err := fmt.Fprintln(stdout, status); err != nil {
			report(stderr, "spillway: init: write setup summary: %v\n", err)
			return 1
		}
	}
	if _, err := lipgloss.Fprintf(stdout, "%s\n", result.Success); err != nil {
		report(stderr, "spillway: init: write setup summary: %v\n", err)
		return 1
	}
	return 0
}

func isInitWizardTerminal(stdin io.Reader, stdout io.Writer) bool {
	if os.Getenv("TERM") == "dumb" || os.Getenv("SPILLWAY_ACCESSIBLE") == "1" {
		return false
	}
	return interactiveTerminal(stdin, stdout)
}
