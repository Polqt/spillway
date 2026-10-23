package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"
)

const maxConfigListFileSize = 1 << 20

func addConfigCommands(root *cobra.Command, exitCode *int, ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, lookupEnv func(string) (string, bool), interactive bool) {
	command := &cobra.Command{
		Use:     "config",
		Short:   "list and open local config files",
		GroupID: "tools",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if commandTUIEnabled(cmd, interactive) {
				if err := runConfigBrowser(ctx, stdin, stdout); err != nil {
					return commandFailure{code: 1, err: err}
				}
				return nil
			}
			return cmd.Help()
		},
	}
	command.AddGroup(&cobra.Group{ID: "config", Title: "Config files"})
	command.AddCommand(&cobra.Command{
		Use:     "list",
		Short:   "list Spillway config files in the current directory",
		GroupID: "config",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if commandTUIEnabled(cmd, interactive) {
				if err := runConfigBrowser(ctx, stdin, stdout); err != nil {
					return commandFailure{code: 1, err: err}
				}
				return nil
			}
			*exitCode = configListCommand(stdout, stderr)
			return nil
		},
	})
	open := &cobra.Command{
		Use:     "open",
		Short:   "open a config file in your text editor",
		GroupID: "config",
		Args:    cobra.NoArgs,
	}
	open.Flags().String("config", defaultConfigPath, "path to the config file to open")
	open.RunE = func(cmd *cobra.Command, _ []string) error {
		path, _ := cmd.Flags().GetString("config")
		*exitCode = configOpenCommand(ctx, path, stdin, stdout, stderr, lookupEnv)
		return nil
	}
	command.AddCommand(open)
	root.AddCommand(command)
}

func configListCommand(stdout, stderr io.Writer) int {
	paths, err := listConfigPaths()
	if err != nil {
		report(stderr, "spillway: config list: read current directory: %v\n", err)
		return 1
	}
	if len(paths) == 0 {
		if _, err := fmt.Fprintln(stdout, "No Spillway config files found in the current directory."); err != nil {
			report(stderr, "spillway: config list: write output: %v\n", err)
			return 1
		}
		return 0
	}
	for _, path := range paths {
		if _, err := fmt.Fprintln(stdout, path); err != nil {
			report(stderr, "spillway: config list: write output: %v\n", err)
			return 1
		}
	}
	return 0
}

func listConfigPaths() ([]string, error) {
	entries, err := os.ReadDir(".")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, entry := range entries {
		if entry.IsDir() || !isConfigCandidate(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxConfigListFileSize {
			continue
		}
		data, err := os.ReadFile(entry.Name())
		if err != nil || !isSpillwayConfig(data) {
			continue
		}
		paths = append(paths, entry.Name())
	}
	return paths, nil
}

func runConfigBrowser(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
	paths, err := listConfigPaths()
	if err != nil {
		return fmt.Errorf("read current directory: %w", err)
	}
	entries := make([]helpEntry, 0, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("inspect config %s: %w", path, err)
		}
		entries = append(entries, helpEntry{
			name:   path,
			group:  "Config file",
			short:  fmt.Sprintf("%d bytes", info.Size()),
			detail: fmt.Sprintf("Path: %s\nSize: %d bytes\nModified: %s", path, info.Size(), info.ModTime().Format(time.RFC3339)),
		})
	}
	return runBrowser(ctx, "config files", "Browse local configs. Use config open to edit one.", entries, stdin, stdout)
}

func isConfigCandidate(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == "" || ext == ".yaml" || ext == ".yml"
}

func isSpillwayConfig(data []byte) bool {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil || len(document.Content) != 1 {
		return false
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return false
	}
	var hasServer, hasProviders bool
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := root.Content[i]
		if key.Kind != yaml.ScalarNode {
			continue
		}
		switch key.Value {
		case "server":
			hasServer = true
		case "providers":
			hasProviders = true
		}
	}
	return hasServer && hasProviders
}

func configOpenCommand(ctx context.Context, path string, stdin io.Reader, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) int {
	info, err := os.Stat(path)
	if err != nil {
		report(stderr, "spillway: config open: %v\nCheck the path or run `spillway init -config %s` to create a config.\n", err, path)
		return 1
	}
	if !info.Mode().IsRegular() {
		report(stderr, "spillway: config open: %s is not a regular file\n", path)
		return 1
	}
	program, args, err := configEditor(path, lookupEnv)
	if err != nil {
		report(stderr, "spillway: config open: %v\n", err)
		return 1
	}
	// #nosec G204 -- the editor is explicitly selected by the user and invoked without a shell.
	command := exec.CommandContext(ctx, program, args...)
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		report(stderr, "spillway: config open: editor failed: %v\n", err)
		return 1
	}
	return 0
}

func configEditor(path string, lookupEnv func(string) (string, bool)) (string, []string, error) {
	for _, name := range []string{"VISUAL", "EDITOR"} {
		if value, ok := lookupEnv(name); ok && strings.TrimSpace(value) != "" {
			program := strings.TrimSpace(value)
			resolved, err := exec.LookPath(program)
			if err != nil {
				return "", nil, fmt.Errorf("editor %q was not found; set VISUAL or EDITOR to an executable path", program)
			}
			return resolved, []string{path}, nil
		}
	}

	program, args := defaultConfigEditor(path)
	resolved, err := exec.LookPath(program)
	if err != nil {
		return "", nil, fmt.Errorf("no text editor was found; set VISUAL or EDITOR to an executable path: %w", err)
	}
	return resolved, args, nil
}

func defaultConfigEditor(path string) (string, []string) {
	switch runtime.GOOS {
	case "windows":
		return "notepad.exe", []string{path}
	case "darwin":
		return "open", []string{"-t", path}
	default:
		return "xdg-open", []string{path}
	}
}
