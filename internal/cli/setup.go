package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Polqt/spillway/internal/config"
	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"
)

type clientSetup struct {
	configPath string
	key        string
	endpoint   string
	executable string
	home       string
}

func setupCommand(ctx context.Context, options Options) *cobra.Command {
	command := &cobra.Command{Use: "setup", Short: "connect a coding client to Spillway", Args: cobra.NoArgs}
	for _, name := range []string{"opencode", "claude", "codex"} {
		client := name
		subcommand := &cobra.Command{
			Use:   client,
			Short: "configure " + client + " to use the local Spillway gateway",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				configPath, _ := cmd.Flags().GetString("config")
				if err := configureClient(ctx, client, configPath, options); err != nil {
					return commandFailure{code: 1, err: err}
				}
				return nil
			},
		}
		subcommand.Flags().String("config", defaultConfigPath, "Spillway config containing the local default key")
		command.AddCommand(subcommand)
	}
	command.GroupID = "manage"
	return command
}

func configureClient(ctx context.Context, client, configPath string, options Options) error {
	setup, err := prepareClientSetup(client, configPath, options)
	if err != nil {
		return err
	}
	var target string
	switch client {
	case "opencode":
		target = filepath.Join(setup.home, ".config", "opencode", "opencode.json")
		err = configureOpenCode(ctx, setup, target, options)
	case "claude":
		target = filepath.Join(setup.home, ".claude", "settings.json")
		err = configureClaude(ctx, setup, target, options)
	case "codex":
		dir := setup.home
		if codexHome, ok := options.LookupEnv("CODEX_HOME"); ok && strings.TrimSpace(codexHome) != "" {
			dir = codexHome
		}
		target = filepath.Join(dir, "spillway.config.toml")
		err = configureCodex(ctx, setup, target, options)
	default:
		return fmt.Errorf("unsupported client %q", client)
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(options.Stdout, "Configured %s to use Spillway.\nConfig: %s\nStart Spillway with: spillway serve -config %s\n", client, target, commandQuote(setup.configPath))
	if err == nil {
		_, err = fmt.Fprintf(options.Stdout, "Next: %s\n", clientLaunchCommand(client))
	}
	return err
}

func clientLaunchCommand(client string) string {
	if client == "codex" {
		return "codex --profile spillway"
	}
	return client
}

func prepareClientSetup(client, configPath string, options Options) (clientSetup, error) {
	home, err := userHome(options.LookupEnv)
	if err != nil {
		return clientSetup{}, err
	}
	configPath, err = findSpillwayConfig(configPath, home)
	if err != nil {
		return clientSetup{}, err
	}
	keys, err := config.LoadLocalKeysWithEnv(configPath, options.LookupEnv)
	if err != nil {
		return clientSetup{}, fmt.Errorf("setup: load local keys: %w", err)
	}
	var localKey string
	defaultKeyFound := false
	for _, key := range keys {
		if key.Name == "default" {
			if defaultKeyFound {
				return clientSetup{}, errors.New("setup: config contains multiple local keys named default")
			}
			defaultKeyFound = true
			localKey = key.Key.Reveal()
		}
	}
	if localKey == "" {
		return clientSetup{}, errors.New("setup: config must contain a non-empty local key named default")
	}
	listen, err := setupListenAddress(configPath)
	if err != nil {
		return clientSetup{}, err
	}
	listenHost, listenPort, err := net.SplitHostPort(listen)
	if err != nil {
		return clientSetup{}, fmt.Errorf("setup: invalid server.listen address: %w", err)
	}
	if listenHost == "" || listenHost == "0.0.0.0" || listenHost == "::" {
		listenHost = "127.0.0.1"
	}
	ip := net.ParseIP(listenHost)
	if ip == nil || !ip.IsLoopback() {
		return clientSetup{}, errors.New("setup: coding clients can only be configured for a loopback server.listen address")
	}
	endpoint := (&url.URL{Scheme: "http", Host: net.JoinHostPort(listenHost, listenPort), Path: "/v1"}).String()
	executable, err := options.Executable()
	if err != nil {
		return clientSetup{}, fmt.Errorf("setup: locate Spillway executable: %w", err)
	}
	configPath, err = filepath.Abs(configPath)
	if err != nil {
		return clientSetup{}, fmt.Errorf("setup: resolve config path: %w", err)
	}
	return clientSetup{configPath: configPath, key: localKey, endpoint: endpoint, executable: executable, home: home}, nil
}

func setupListenAddress(path string) (string, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- explicit Spillway config selected for client setup.
	if err != nil {
		return "", fmt.Errorf("setup: read Spillway config: %w", err)
	}
	var parsed struct {
		Server struct {
			Listen string `yaml:"listen"`
		} `yaml:"server"`
	}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return "", errors.New("setup: Spillway config contains invalid YAML")
	}
	if parsed.Server.Listen == "" {
		return "127.0.0.1:8080", nil
	}
	return parsed.Server.Listen, nil
}

func userHome(lookup func(string) (string, bool)) (string, error) {
	for _, name := range []string{"USERPROFILE", "HOME"} {
		if value, ok := lookup(name); ok && strings.TrimSpace(value) != "" {
			return filepath.Clean(value), nil
		}
	}
	return "", errors.New("setup: user home directory is not set")
}

func findSpillwayConfig(configPath, home string) (string, error) {
	if configPath != defaultConfigPath {
		return configPath, nil
	}
	if info, err := os.Stat(configPath); err == nil && info.Mode().IsRegular() {
		return configPath, nil
	}
	homeConfig := filepath.Join(home, defaultConfigPath)
	if info, err := os.Stat(homeConfig); err == nil && info.Mode().IsRegular() {
		return homeConfig, nil
	}
	return "", fmt.Errorf("setup: config.yaml was not found in the current directory or %s; pass -config <path>", home)
}

func configureOpenCode(ctx context.Context, setup clientSetup, path string, options Options) error {
	settings, err := readJSONSettings(path)
	if err != nil {
		return err
	}
	provider, _ := settings["provider"].(map[string]any)
	if provider == nil {
		provider = make(map[string]any)
	}
	spillway, _ := provider["spillway"].(map[string]any)
	if spillway == nil {
		spillway = make(map[string]any)
	}
	spillway["npm"] = "@ai-sdk/openai-compatible"
	spillway["name"] = "Spillway"
	spillway["options"] = mergeObject(spillway["options"], map[string]any{"baseURL": setup.endpoint, "apiKey": "{file:./spillway.key}"})
	spillway["models"] = mergeObject(spillway["models"], map[string]any{"auto": map[string]any{"name": "Spillway automatic routing"}})
	provider["spillway"] = spillway
	settings["provider"] = provider
	settings["model"] = "spillway/auto"
	keyPath := filepath.Join(filepath.Dir(path), "spillway.key")
	if err := writePrivateText(ctx, keyPath, []byte(setup.key), options); err != nil {
		return fmt.Errorf("setup: save private OpenCode credential file: %w", err)
	}
	return writeJSONSettings(ctx, path, settings, options)
}

func configureClaude(ctx context.Context, setup clientSetup, path string, options Options) error {
	settings, err := readJSONSettings(path)
	if err != nil {
		return err
	}
	baseURL := strings.TrimSuffix(setup.endpoint, "/v1")
	env := mergeObject(settings["env"], map[string]any{"ANTHROPIC_BASE_URL": baseURL, "ANTHROPIC_MODEL": "auto"})
	settings["env"] = env
	settings["apiKeyHelper"] = shellHelper(setup.executable, setup.configPath)
	return writeJSONSettings(ctx, path, settings, options)
}

func configureCodex(ctx context.Context, setup clientSetup, path string, options Options) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("setup: create Codex config directory: %w", err)
	}
	command := strconv.Quote(setup.executable)
	args := []string{"key", "-config", setup.configPath, "default"}
	quotedArgs := make([]string, len(args))
	for i, arg := range args {
		quotedArgs[i] = strconv.Quote(arg)
	}
	content := strings.Join([]string{
		"# Managed by Spillway. Re-run `spillway setup codex` to update.",
		"model = \"auto\"",
		"model_provider = \"spillway\"",
		"approval_policy = \"on-request\"",
		"sandbox_mode = \"workspace-write\"",
		"",
		"[model_providers.spillway]",
		"name = \"Spillway\"",
		"base_url = " + strconv.Quote(setup.endpoint),
		"wire_api = \"responses\"",
		"",
		"[model_providers.spillway.auth]",
		"command = " + command,
		"args = [" + strings.Join(quotedArgs, ", ") + "]",
		"",
	}, "\n")
	if existing, err := os.ReadFile(path); err == nil && !strings.HasPrefix(string(existing), "# Managed by Spillway.") { // #nosec G304 -- path is the user's explicit CODEX_HOME or default home profile.
		return fmt.Errorf("setup: %s already exists and is not managed by Spillway; move it aside before setup", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("setup: read Codex profile: %w", err)
	}
	return writePrivateText(ctx, path, []byte(content), options)
}

func shellHelper(executable, configPath string) string {
	return shellQuote(executable) + " key -config " + shellQuote(configPath) + " default"
}

func shellQuote(value string) string {
	return "\"" + strings.ReplaceAll(value, "\"", "\\\"") + "\""
}

func commandQuote(value string) string {
	return "\"" + strings.ReplaceAll(value, "\"", "\"\"") + "\""
}

func readJSONSettings(path string) (map[string]any, error) {
	settings := make(map[string]any)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return settings, nil
	}
	if err != nil {
		return nil, fmt.Errorf("setup: inspect client settings %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("setup: client settings %s must be a regular file, not a symlink", path)
	}
	if info.Size() > 1<<20 {
		return nil, fmt.Errorf("setup: client settings %s are larger than 1 MiB", path)
	}
	data, err := os.ReadFile(path) // #nosec G304 -- a fixed global client settings path under the user's home directory.
	if err != nil {
		return nil, fmt.Errorf("setup: read client settings %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, fmt.Errorf("setup: client settings %s contain invalid JSON", path)
	}
	return settings, nil
}

func mergeObject(current any, additions map[string]any) map[string]any {
	result, _ := current.(map[string]any)
	if result == nil {
		result = make(map[string]any)
	}
	for key, value := range additions {
		result[key] = value
	}
	return result
}

func writeJSONSettings(ctx context.Context, path string, settings map[string]any, options Options) error {
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("setup: encode client settings: %w", err)
	}
	data = append(data, '\n')
	return writePrivateText(ctx, path, data, options)
}

func writePrivateText(ctx context.Context, path string, content []byte, options Options) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("setup: create client config directory: %w", err)
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("setup: client settings target must be a regular file, not a symlink")
		}
		current, err := os.ReadFile(path) // #nosec G304 -- path is a global client config or its adjacent private key file.
		if err != nil {
			return fmt.Errorf("setup: read existing client settings: %w", err)
		}
		if bytes.Equal(current, content) {
			return nil
		}
		_, err = config.ReplaceContents(ctx, path, content, config.CommitOptions{Now: options.Now})
		if err != nil {
			return fmt.Errorf("setup: update client settings: %w", err)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("setup: inspect client settings: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- path is a fixed client config or adjacent private key under the user's home directory.
	if err != nil {
		return fmt.Errorf("setup: create client settings: %w", err)
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return fmt.Errorf("setup: write client settings: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return fmt.Errorf("setup: sync client settings: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("setup: close client settings: %w", err)
	}
	return nil
}
