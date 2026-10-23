package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"charm.land/glamour/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	charmLog "charm.land/log/v2"
	"github.com/Polqt/spillway/internal/config"
	"github.com/Polqt/spillway/internal/providers"
	glowui "github.com/charmbracelet/glow/v2/ui"
	glowLog "github.com/charmbracelet/log"
	"golang.org/x/term"
)

func initCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath, "path for the new config file")
	providerFlag := fs.String("providers", "", "comma-separated providers to configure")
	attestFlag := fs.String("attest-free-tier", "", "comma-separated providers attested as zero-spend")
	storeFlag := fs.Bool("store", false, "enable the local usage store and dashboard key")
	yes := fs.Bool("yes", false, "accept defaults and run without prompts")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		report(stderr, "spillway: init: unexpected argument %q\n\n%s", fs.Arg(0), usage)
		return 2
	}
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	inputCloser, _ := stdin.(io.Closer)
	provided := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { provided[f.Name] = true })
	if !*yes && isInitWizardTerminal(stdin, stdout) && !provided["providers"] && !provided["attest-free-tier"] && !provided["store"] {
		return runInitWizard(ctx, *configPath, lookupEnv, stdin, stdout, stderr)
	}
	if !*yes {
		if err := writeInitBanner(stdout); err != nil {
			report(stderr, "spillway: init: write setup heading: %v\n", err)
			return 1
		}
	}
	targetConfigPath := *configPath
	var err error
	if provided["config"] || *yes {
		if err := validateNewConfigPath(targetConfigPath); err != nil {
			if errors.Is(err, os.ErrExist) {
				reportInitError(stderr, "That config file already exists", fmt.Sprintf("%s already exists. Spillway never overwrites existing configs.", targetConfigPath), fmt.Sprintf("Check it with `spillway check -config %s`, add a provider with `spillway provider add`, or choose a new path with `spillway init -config <new-path>`.", targetConfigPath))
			} else {
				reportInitError(stderr, "Couldn't use that config path", err.Error(), "Choose a writable file path and try `spillway init` again.")
			}
			return 1
		}
	} else {
		targetConfigPath, err = chooseConfigPathInteractive(ctx, stdin, inputCloser, stdout)
		if err != nil {
			return initChoiceError(stderr, err)
		}
	}

	var selected []string
	if provided["providers"] || *yes {
		selected, err = chooseProviders(ctx, *providerFlag, provided["providers"], *yes, bufio.NewReader(stdin), inputCloser, stdout)
	} else {
		selected, err = chooseProvidersInteractive(ctx, stdin, inputCloser, stdout)
	}
	if err != nil {
		return initChoiceError(stderr, err)
	}
	var attested map[string]bool
	if provided["attest-free-tier"] || *yes {
		attested, err = chooseAttestations(ctx, selected, *attestFlag, provided["attest-free-tier"], *yes, bufio.NewReader(stdin), inputCloser, stdout)
	} else {
		attested, err = chooseAttestationsInteractive(ctx, selected, stdin, inputCloser, stdout)
	}
	if err != nil {
		return initChoiceError(stderr, err)
	}
	models := eligibleModels(selected, attested)
	if !*yes && len(models) > 0 {
		models, err = chooseModelsInteractive(ctx, models, stdin, inputCloser, stdout)
		if err != nil {
			return initChoiceError(stderr, err)
		}
	}
	var storeEnabled bool
	if provided["store"] || *yes {
		storeEnabled, err = chooseStore(ctx, *storeFlag, provided["store"], *yes, bufio.NewReader(stdin), inputCloser, stdout)
	} else {
		storeEnabled, err = chooseStoreInteractive(ctx, stdin, inputCloser, stdout)
	}
	if err != nil {
		return initChoiceError(stderr, err)
	}

	content, statuses, err := buildInitConfig(selected, attested, models, storeEnabled, lookupEnv)
	if err != nil {
		reportInitError(stderr, "Couldn't prepare the config", err.Error(), "Review the selected providers and environment values, then try `spillway init` again.")
		return 1
	}
	if err := writeInitConfig(ctx, targetConfigPath, content); err != nil {
		if errors.Is(err, os.ErrExist) {
			reportInitError(stderr, "That config file already exists", fmt.Sprintf("%s already exists. Spillway never overwrites existing configs.", targetConfigPath), fmt.Sprintf("Check it with `spillway check -config %s`, add a provider with `spillway provider add`, or choose a new path with `spillway init -config <new-path>`.", targetConfigPath))
		} else {
			reportInitError(stderr, "Couldn't create the config file", fmt.Sprintf("`%s`: %v", targetConfigPath, err), "Check that the folder exists and is writable, or choose another path.")
		}
		return 1
	}
	baseURL := "http:" + "/" + "/127.0.0.1:8080"
	createdStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2"))
	labelStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	keyCommand := "spillway key"
	if targetConfigPath != defaultConfigPath {
		keyCommand += " -config \"" + targetConfigPath + "\""
	}
	if interactiveTerminal(stdin, stdout) {
		if err := writeInitSummary(stdout, targetConfigPath, statuses, attested, selected, storeEnabled, baseURL, keyCommand); err == nil {
			return 0
		}
		_, _ = fmt.Fprintf(stderr, "Note: the config is ready, but the styled summary could not render.\n")
	}
	for _, status := range statuses {
		name, value, _ := strings.Cut(status, ": ")
		valueStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
		if value == "not set" {
			valueStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
		}
		if _, err := lipgloss.Fprintf(stdout, "%s: %s\n", name, valueStyle.Render(value)); err != nil {
			report(stderr, "spillway: init: write next steps: %v\n", err)
			return 1
		}
		if strings.HasSuffix(status, ": not set") {
			variable := strings.TrimSuffix(status, ": not set")
			if _, err := fmt.Fprintf(stdout, "Set it in this shell: %s\n", environmentRecoveryCommand(variable)); err != nil {
				report(stderr, "spillway: init: write next steps: %v\n", err)
				return 1
			}
		}
	}
	if len(attested) < len(selected) {
		warningStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
		for _, name := range selected {
			preset, _ := providers.LookupPreset(name)
			if preset.FreeTierAttestable && !attested[name] {
				if _, err := lipgloss.Fprintf(stdout, "%s %s is configured but omitted from tiers.free until you attest that its account or project has no paid plan.\n", warningStyle.Render("Note:"), name); err != nil {
					report(stderr, "spillway: init: write next steps: %v\n", err)
					return 1
				}
			}
		}
	}
	if _, err := lipgloss.Fprintf(stdout, "%s %s (keep private).\n", createdStyle.Render("Created"), targetConfigPath); err != nil {
		report(stderr, "spillway: init: write next steps: %v\n", err)
		return 1
	}
	if _, err := lipgloss.Fprintf(stdout, "%s base URL %s/v1; model `auto`; key `%s default`.\n", labelStyle.Render("Client:"), baseURL, keyCommand); err != nil {
		report(stderr, "spillway: init: write next steps: %v\n", err)
		return 1
	}
	if storeEnabled {
		if _, err := lipgloss.Fprintf(stdout, "%s page %s/dashboard; key `%s dashboard`.\n", labelStyle.Render("Dashboard:"), baseURL, keyCommand); err != nil {
			report(stderr, "spillway: init: write next steps: %v\n", err)
			return 1
		}
	} else if _, err := lipgloss.Fprintf(stdout, "%s %s/dashboard (rerun init with `-store` to enable login).\n", labelStyle.Render("Dashboard:"), baseURL); err != nil {
		report(stderr, "spillway: init: write next steps: %v\n", err)
		return 1
	}
	if _, err := lipgloss.Fprintf(stdout, "%s `spillway check -config %s`, then `spillway serve -config %s`.\n", labelStyle.Render("Start:"), targetConfigPath, targetConfigPath); err != nil {
		report(stderr, "spillway: init: write next steps: %v\n", err)
		return 1
	}
	return 0
}

func interactiveTerminal(stdin io.Reader, stdout io.Writer) bool {
	in, inputOK := stdin.(*os.File)
	out, outputOK := stdout.(*os.File)
	return inputOK && outputOK && in == os.Stdin && out == os.Stdout && term.IsTerminal(int(in.Fd())) && term.IsTerminal(int(out.Fd()))
}

func writeInitSummary(stdout io.Writer, configPath string, statuses []string, attested map[string]bool, selected []string, storeEnabled bool, baseURL, keyCommand string) (err error) {
	var markdown strings.Builder
	markdown.WriteString("# Spillway is ready\n\n")
	markdown.WriteString("Your local gateway is set up. Here's everything you need to get connected.\n\n")
	markdown.WriteString("## Config\n\n`" + configPath + "` was created with private file permissions.\n\n")
	markdown.WriteString("## Providers\n\n")
	for _, name := range selected {
		preset, _ := providers.LookupPreset(name)
		if preset.KeyEnv == "" {
			markdown.WriteString("- **" + name + ":** ready\n")
			continue
		}
		status := "not set"
		for _, value := range statuses {
			if strings.HasPrefix(value, preset.KeyEnv+": ") {
				status = strings.TrimPrefix(value, preset.KeyEnv+": ")
				break
			}
		}
		markdown.WriteString("- **" + name + ":** `" + preset.KeyEnv + "` " + status + "\n")
	}
	markdown.WriteString("\n")
	if len(statuses) > 0 {
		for _, status := range statuses {
			if strings.HasSuffix(status, ": not set") {
				variable := strings.TrimSuffix(status, ": not set")
				markdown.WriteString("Set it in this shell: `" + environmentRecoveryCommand(variable) + "`\n\n")
			}
		}
	}
	for _, name := range selected {
		preset, _ := providers.LookupPreset(name)
		if preset.FreeTierAttestable && !attested[name] {
			markdown.WriteString("> **Note:** " + name + " is configured but excluded from the free tier until you attest that its account or project has no paid plan.\n\n")
		}
	}
	markdown.WriteString("## Connect your app\n\n")
	markdown.WriteString("- **Base URL:** `" + baseURL + "/v1`\n")
	markdown.WriteString("- **Model:** `auto`\n")
	markdown.WriteString("- **API key:** run `" + keyCommand + " default`\n\n")
	if storeEnabled {
		markdown.WriteString("## Local dashboard\n\nOpen `" + baseURL + "/dashboard`. Get its key with `" + keyCommand + " dashboard`.\n\n")
	}
	markdown.WriteString("## Start the gateway\n\n```sh\nspillway check -config " + configPath + "\nspillway serve -config " + configPath + "\n```")
	summaryFile, err := os.CreateTemp(".", "spillway-init-summary-*.md")
	if err != nil {
		return fmt.Errorf("create Glow summary: %w", err)
	}
	summaryPath := summaryFile.Name()
	defer func() {
		if removeErr := os.Remove(summaryPath); err == nil && removeErr != nil {
			err = fmt.Errorf("remove temporary Glow summary: %w", removeErr)
		}
	}()
	if _, err := io.WriteString(summaryFile, markdown.String()); err != nil {
		closeErr := summaryFile.Close()
		return fmt.Errorf("write Glow summary: %w", errors.Join(err, closeErr))
	}
	if err := summaryFile.Close(); err != nil {
		return fmt.Errorf("close Glow summary: %w", err)
	}
	program := glowui.NewProgram(glowui.Config{Path: summaryPath, GlamourEnabled: true, GlamourMaxWidth: 88, GlamourStyle: "dark"}, "")
	previousLogLevel := glowLog.GetLevel()
	glowLog.SetLevel(glowLog.ErrorLevel)
	defer glowLog.SetLevel(previousLogLevel)
	_, err = program.Run()
	return err
}

func writeInitBanner(stdout io.Writer) error {
	title := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Render("SPILLWAY / setup")
	subtitle := lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Render("Your local AI gateway, ready in a few steps")
	banner := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("57")).Padding(0, 1).Render(title + "\n" + subtitle)
	_, err := lipgloss.Fprintln(stdout, banner+"\n")
	return err
}

func chooseConfigPathInteractive(ctx context.Context, stdin io.Reader, closeInput io.Closer, stdout io.Writer) (string, error) {
	path := defaultConfigPath
	field := huh.NewInput().
		Title("Config file").
		Description("Choose where Spillway creates the new config.").
		Value(&path).
		Validate(validateNewConfigPath)
	if err := runInitForm(ctx, closeInput, stdin, stdout, huh.NewForm(huh.NewGroup(field))); err != nil {
		return "", err
	}
	return path, nil
}

func validateNewConfigPath(path string) error {
	if path == "" {
		return errors.New("config path must not be empty")
	}
	_, err := os.Lstat(path)
	if err == nil {
		return fmt.Errorf("%w: %s already exists; choose another path", os.ErrExist, path)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check config path: %w", err)
	}
	directory := filepath.Dir(path)
	info, err := os.Stat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("config folder %q does not exist; create it first or choose a path in an existing folder", directory)
	}
	if err != nil {
		return fmt.Errorf("check config folder: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("config folder %q is not a directory; choose a path in an existing folder", directory)
	}
	return nil
}

func chooseProvidersInteractive(ctx context.Context, stdin io.Reader, closeInput io.Closer, stdout io.Writer) ([]string, error) {
	selected := []string{"mock"}
	options := make([]huh.Option[string], 0, len(providers.Presets()))
	for _, preset := range providers.Presets() {
		options = append(options, huh.NewOption(preset.Name, preset.Name))
	}
	field := huh.NewMultiSelect[string]().
		Title("Choose providers").
		Description("Select the providers Spillway may use.").
		Options(options...).
		Value(&selected).
		Validate(func(values []string) error {
			if len(values) == 0 {
				return initUsageError{message: "select at least one provider"}
			}
			return nil
		})
	if err := runInitForm(ctx, closeInput, stdin, stdout, huh.NewForm(huh.NewGroup(field))); err != nil {
		return nil, err
	}
	return parseProviderList(strings.Join(selected, ","))
}

func chooseModelsInteractive(ctx context.Context, suggestions []string, stdin io.Reader, closeInput io.Closer, stdout io.Writer) ([]string, error) {
	selected := slices.Clone(suggestions)
	options := make([]huh.Option[string], 0, len(suggestions))
	for _, model := range suggestions {
		options = append(options, huh.NewOption(model, model))
	}
	field := huh.NewMultiSelect[string]().
		Title("Choose models for auto").
		Description("Select the suggested models to include in the free tier.").
		Options(options...).
		Value(&selected).
		Validate(func(values []string) error {
			if len(values) == 0 {
				return initUsageError{message: "select at least one model"}
			}
			return nil
		})
	if err := runInitForm(ctx, closeInput, stdin, stdout, huh.NewForm(huh.NewGroup(field))); err != nil {
		return nil, err
	}
	return selected, nil
}

func chooseAttestationsInteractive(ctx context.Context, selected []string, stdin io.Reader, closeInput io.Closer, stdout io.Writer) (map[string]bool, error) {
	attested := make(map[string]bool)
	fields := make([]huh.Field, 0, len(selected))
	accepted := make(map[string]*bool, len(selected))
	for _, name := range selected {
		preset, _ := providers.LookupPreset(name)
		if !preset.FreeTierAttestable {
			continue
		}
		value := new(bool)
		accepted[name] = value
		fields = append(fields, huh.NewConfirm().
			Title("Attest that "+name+" has no paid plan?").
			Description("Only attest if this provider account or project has no paid plan.").
			Affirmative("Yes").Negative("No").Value(value))
	}
	if len(fields) == 0 {
		return attested, nil
	}
	if err := runInitForm(ctx, closeInput, stdin, stdout, huh.NewForm(huh.NewGroup(fields...))); err != nil {
		return nil, err
	}
	for name, value := range accepted {
		if *value {
			attested[name] = true
		}
	}
	return attested, nil
}

func chooseStoreInteractive(ctx context.Context, stdin io.Reader, closeInput io.Closer, stdout io.Writer) (bool, error) {
	enabled := false
	field := huh.NewConfirm().
		Title("Enable local usage history and dashboard access?").
		Description("Usage metadata stays in the local SQLite database.").
		Affirmative("Yes").Negative("No").Value(&enabled)
	if err := runInitForm(ctx, closeInput, stdin, stdout, huh.NewForm(huh.NewGroup(field))); err != nil {
		return false, err
	}
	return enabled, nil
}

type singleByteReader struct {
	reader io.Reader
	sawEOF bool
}

func (r *singleByteReader) Read(buffer []byte) (int, error) {
	// Huh creates a scanner per field, so prevent one field from consuming later answers.
	if len(buffer) > 1 {
		buffer = buffer[:1]
	}
	n, err := r.reader.Read(buffer)
	if errors.Is(err, io.EOF) {
		r.sawEOF = true
	}
	return n, err
}

func runInitForm(ctx context.Context, closeInput io.Closer, stdin io.Reader, stdout io.Writer, form *huh.Form) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	input := &singleByteReader{reader: stdin}
	form.WithAccessible(true).WithInput(input).WithOutput(stdout)
	done := make(chan struct{})
	if closeInput != nil {
		go func() {
			select {
			case <-ctx.Done():
				_ = closeInput.Close()
			case <-done:
			}
		}()
	}
	err := form.RunWithContext(ctx)
	close(done)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if input.sawEOF {
		return errors.New("unexpected end of input")
	}
	if err != nil {
		return fmt.Errorf("run interactive setup: %w", err)
	}
	return nil
}

func initChoiceError(stderr io.Writer, err error) int {
	var usageErr initUsageError
	if errors.As(err, &usageErr) {
		reportInitError(stderr, "One of the setup choices needs attention", err.Error(), "Choose a listed option, then continue setup.")
		return 2
	}
	if errors.Is(err, context.Canceled) {
		reportInitError(stderr, "Setup was cancelled", "No config file was created.", "Run `spillway init` when you're ready to continue.")
		return 1
	}
	if strings.Contains(err.Error(), "unexpected end of input") {
		reportInitError(stderr, "Setup needs more input", "The prompt ended before all choices were complete, so no config file was created.", "Run `spillway init` in a terminal, or use `spillway init -yes -providers mock` for non-interactive setup.")
		return 1
	}
	reportInitError(stderr, "Setup couldn't continue", err.Error(), "Run `spillway init --help` to review the available options.")
	return 1
}

func reportInitError(stderr io.Writer, title, detail, suggestion string) {
	logger := charmLog.NewWithOptions(stderr, charmLog.Options{Prefix: "spillway", ReportTimestamp: false})
	logger.Error(title)
	style := glamour.WithEnvironmentConfig()
	if _, noColor := os.LookupEnv("NO_COLOR"); noColor || os.Getenv("TERM") == "dumb" {
		style = glamour.WithStandardStyle("notty")
	}
	renderer, err := glamour.NewTermRenderer(style, glamour.WithWordWrap(78))
	if err == nil {
		if rendered, renderErr := renderer.Render("**What happened**\n"); renderErr == nil {
			_, _ = io.WriteString(stderr, rendered)
		}
	}
	_, _ = fmt.Fprintf(stderr, "%s\n", detail)
	if err == nil {
		if rendered, renderErr := renderer.Render("\n**Next step**\n"); renderErr == nil {
			_, _ = io.WriteString(stderr, rendered)
		}
	} else {
		_, _ = io.WriteString(stderr, "Next step:\n")
	}
	_, _ = fmt.Fprintf(stderr, "%s\n", suggestion)
}

type initUsageError struct{ message string }

func (e initUsageError) Error() string { return e.message }

func chooseProviders(ctx context.Context, value string, explicitlySet, yes bool, stdin *bufio.Reader, closeInput io.Closer, stdout io.Writer) ([]string, error) {
	if explicitlySet {
		return parseProviderList(value)
	}
	if yes {
		return nil, initUsageError{message: "-yes requires -providers"}
	}
	answer, err := promptLine(ctx, stdin, closeInput, stdout, "Providers (comma-separated: "+providerNames()+") [mock]: ")
	if err != nil {
		return nil, err
	}
	if answer == "" {
		answer = "mock"
	}
	return parseProviderList(answer)
}

func chooseAttestations(ctx context.Context, selected []string, value string, explicitlySet, yes bool, stdin *bufio.Reader, closeInput io.Closer, stdout io.Writer) (map[string]bool, error) {
	attested := make(map[string]bool)
	if explicitlySet {
		if strings.TrimSpace(value) == "" {
			return attested, nil
		}
		requested, err := parseProviderList(value)
		if err != nil {
			return nil, err
		}
		for _, name := range requested {
			preset, _ := providers.LookupPreset(name)
			if !slices.Contains(selected, name) || !preset.FreeTierAttestable {
				return nil, fmt.Errorf("-attest-free-tier accepts only selected attestable providers (groq, gemini)")
			}
			attested[name] = true
		}
		return attested, nil
	}
	for _, name := range selected {
		preset, _ := providers.LookupPreset(name)
		if !preset.FreeTierAttestable || yes {
			continue
		}
		answer, err := promptLine(ctx, stdin, closeInput, stdout, "Attest that "+name+" has no paid plan? [y/N]: ")
		if err != nil {
			return nil, err
		}
		if isYes(answer) {
			attested[name] = true
		}
	}
	return attested, nil
}

func chooseStore(ctx context.Context, enabled, explicitlySet, yes bool, stdin *bufio.Reader, closeInput io.Closer, stdout io.Writer) (bool, error) {
	if explicitlySet || yes {
		return enabled, nil
	}
	answer, err := promptLine(ctx, stdin, closeInput, stdout, "Enable local usage history and dashboard access? [y/N]: ")
	if err != nil {
		return false, err
	}
	return isYes(answer), nil
}

func promptLine(ctx context.Context, stdin *bufio.Reader, closeInput io.Closer, stdout io.Writer, prompt string) (string, error) {
	if _, err := io.WriteString(stdout, prompt); err != nil {
		return "", fmt.Errorf("write prompt: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var answer string
	var err error
	if closeInput == nil {
		answer, err = stdin.ReadString('\n')
	} else {
		type lineResult struct {
			answer string
			err    error
		}
		result := make(chan lineResult, 1)
		go func() {
			answer, err := stdin.ReadString('\n')
			result <- lineResult{answer: answer, err: err}
		}()
		select {
		case <-ctx.Done():
			// Closing stdin releases the owned read when the command is interrupted.
			_ = closeInput.Close()
			return "", ctx.Err()
		case line := <-result:
			answer, err = line.answer, line.err
		}
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read prompt: %w", err)
	}
	if errors.Is(err, io.EOF) && answer == "" {
		return "", errors.New("unexpected end of input")
	}
	return strings.TrimSpace(answer), nil
}

func isYes(answer string) bool {
	return strings.EqualFold(strings.TrimSpace(answer), "y") || strings.EqualFold(strings.TrimSpace(answer), "yes")
}

func parseProviderList(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, initUsageError{message: "-providers must name at least one provider"}
	}
	seen := make(map[string]bool)
	selected := make([]string, 0)
	for _, item := range strings.Split(value, ",") {
		name := strings.TrimSpace(item)
		if name == "" {
			return nil, fmt.Errorf("-providers contains an empty provider name")
		}
		if _, ok := providers.LookupPreset(name); !ok {
			return nil, fmt.Errorf("unknown provider %q; supported: %s", name, providerNames())
		}
		if seen[name] {
			return nil, fmt.Errorf("-providers repeats %q", name)
		}
		seen[name] = true
		selected = append(selected, name)
	}
	slices.Sort(selected)
	return selected, nil
}

func providerNames() string {
	names := make([]string, 0, len(providers.Presets()))
	for _, preset := range providers.Presets() {
		names = append(names, preset.Name)
	}
	return strings.Join(names, ", ")
}

func eligibleModels(selected []string, attested map[string]bool) []string {
	models := make([]string, 0, len(selected))
	for _, name := range selected {
		preset, _ := providers.LookupPreset(name)
		if !preset.FreeTierAttestable || attested[name] {
			models = append(models, name+"/"+preset.SuggestedModel)
		}
	}
	return models
}

func buildInitConfig(selected []string, attested map[string]bool, models []string, storeEnabled bool, lookupEnv func(string) (string, bool)) ([]byte, []string, error) {
	localKey, err := generateLocalKey()
	if err != nil {
		return nil, nil, fmt.Errorf("generate local key: %w", err)
	}
	var dashboardKey string
	if storeEnabled {
		dashboardKey, err = generateLocalKey()
		if err != nil {
			return nil, nil, fmt.Errorf("generate dashboard key: %w", err)
		}
	}
	var out strings.Builder
	out.WriteString("server:\n  listen: 127.0.0.1:8080\n  api_keys:\n    - name: default\n      key: \"")
	out.WriteString(localKey)
	out.WriteString("\"\n")
	if storeEnabled {
		out.WriteString("    - name: dashboard\n      key: \"")
		out.WriteString(dashboardKey)
		out.WriteString("\"\n      dashboard: true\n")
	}
	out.WriteString("providers:\n")
	statuses := make([]string, 0)
	for _, name := range selected {
		preset, _ := providers.LookupPreset(name)
		if preset.KeyEnv != "" {
			out.WriteString("  ")
			out.WriteString(name)
			out.WriteString(":\n    api_key: \"${")
			out.WriteString(preset.KeyEnv)
			out.WriteString("}\"\n")
			if preset.FreeTierAttestable && attested[name] {
				out.WriteString("    free_tier: true\n")
			}
			ok := false
			if lookupEnv != nil {
				_, ok = lookupEnv(preset.KeyEnv)
			}
			label := "not set"
			if ok {
				label = "set"
			}
			statuses = append(statuses, preset.KeyEnv+": "+label)
		} else {
			out.WriteString("  ")
			out.WriteString(name)
			out.WriteString(": {}\n")
		}
	}
	if len(models) > 0 {
		out.WriteString("# Suggested model IDs may be outdated; check current provider documentation.\n")
		out.WriteString("tiers:\n  free:\n    models:\n")
		for _, model := range models {
			out.WriteString("      - ")
			out.WriteString(model)
			out.WriteString("\n")
		}
		out.WriteString("router:\n  auto:\n    - free\n")
	}
	if storeEnabled {
		out.WriteString("store:\n  path: ./spillway.db\n  retention: 90d\n")
	}
	content := []byte(out.String())
	validationLookup := func(string) (string, bool) {
		return "sk-init-validation", true
	}
	if _, err := config.ParseWithEnv(content, validationLookup); err != nil {
		return nil, nil, fmt.Errorf("validate generated config: %w", err)
	}
	return content, statuses, nil
}

func generateLocalKey() (string, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "sk-spillway-" + base64.RawURLEncoding.EncodeToString(random[:]), nil
}

func writeInitConfig(ctx context.Context, path string, content []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// #nosec G304 -- the path is the user's explicit -config destination.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := file.Write(content)
	if err != nil || n != len(content) {
		_ = file.Close()
		_ = os.Remove(path)
		if err != nil {
			return err
		}
		return io.ErrShortWrite
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	if err := ctx.Err(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}
