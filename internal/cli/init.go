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
	"slices"
	"strings"

	"github.com/Polqt/spillway/internal/config"
	"github.com/Polqt/spillway/internal/providers"
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
	input := bufio.NewReader(stdin)
	inputCloser, _ := stdin.(io.Closer)
	provided := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { provided[f.Name] = true })

	selected, err := chooseProviders(ctx, *providerFlag, provided["providers"], *yes, input, inputCloser, stdout)
	if err != nil {
		return initChoiceError(stderr, err)
	}
	attested, err := chooseAttestations(ctx, selected, *attestFlag, provided["attest-free-tier"], *yes, input, inputCloser, stdout)
	if err != nil {
		return initChoiceError(stderr, err)
	}
	storeEnabled, err := chooseStore(ctx, *storeFlag, provided["store"], *yes, input, inputCloser, stdout)
	if err != nil {
		return initChoiceError(stderr, err)
	}

	content, statuses, err := buildInitConfig(selected, attested, storeEnabled, lookupEnv)
	if err != nil {
		report(stderr, "spillway: init: %v\n", err)
		return 1
	}
	if err := writeInitConfig(*configPath, content); err != nil {
		report(stderr, "spillway: init: create %s: %v\n", *configPath, err)
		return 1
	}
	for _, status := range statuses {
		if _, err := fmt.Fprintln(stdout, status); err != nil {
			report(stderr, "spillway: init: write status: %v\n", err)
			return 1
		}
	}
	if len(attested) < len(selected) {
		for _, name := range selected {
			preset, _ := providers.LookupPreset(name)
			if preset.FreeTierAttestable && !attested[name] {
				if _, err := fmt.Fprintf(stdout, "%s is configured but omitted from tiers.free until you attest that its account or project has no paid plan.\n", name); err != nil {
					report(stderr, "spillway: init: write next steps: %v\n", err)
					return 1
				}
			}
		}
	}
	if _, err := fmt.Fprintf(stdout, "Created %s. The generated local key is stored in that file; keep it private.\nRun `spillway serve -config %s` to start Spillway.\n", *configPath, *configPath); err != nil {
		report(stderr, "spillway: init: write next steps: %v\n", err)
		return 1
	}
	return 0
}

func initChoiceError(stderr io.Writer, err error) int {
	var usageErr initUsageError
	if errors.As(err, &usageErr) {
		report(stderr, "spillway: init: %v\n", err)
		return 2
	}
	report(stderr, "spillway: init: %v\n", err)
	return 1
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

func buildInitConfig(selected []string, attested map[string]bool, storeEnabled bool, lookupEnv func(string) (string, bool)) ([]byte, []string, error) {
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
		out.WriteString("    - name: dashboard\n      key: \"" + dashboardKey + "\"\n      dashboard: true\n")
	}
	out.WriteString("providers:\n")
	statuses := make([]string, 0)
	eligible := make([]string, 0, len(selected))
	for _, name := range selected {
		preset, _ := providers.LookupPreset(name)
		if preset.KeyEnv != "" {
			out.WriteString("  " + name + ":\n    api_key: \"${" + preset.KeyEnv + "}\"\n")
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
			out.WriteString("  " + name + ": {}\n")
		}
		if !preset.FreeTierAttestable || attested[name] {
			eligible = append(eligible, name+"/"+preset.SuggestedModel)
		}
	}
	if len(eligible) > 0 {
		out.WriteString("# Suggested model IDs may be outdated; check current provider documentation.\n")
		out.WriteString("tiers:\n  free:\n    models:\n")
		for _, model := range eligible {
			out.WriteString("      - " + model + "\n")
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
