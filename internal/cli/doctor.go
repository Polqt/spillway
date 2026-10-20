package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/Polqt/spillway/apps/spillway/web"
	"github.com/Polqt/spillway/internal/config"
	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/store"
	"github.com/spf13/cobra"
)

func doctorCommand(ctx context.Context, options Options) *cobra.Command {
	var configPath string
	var noProbe bool
	command := &cobra.Command{
		Use:   "doctor",
		Short: "check config, environment and installation (offline)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			failed, err := runDoctor(ctx, cmd.OutOrStdout(), configPath, cmd.Flags().Changed("config"), options, noProbe)
			if err != nil {
				return commandFailure{code: 1, err: err}
			}
			if failed {
				return commandError{code: 1}
			}
			return nil
		},
	}
	command.Flags().StringVar(&configPath, "config", defaultConfigPath, "path to the config file")
	command.Flags().BoolVar(&noProbe, "no-probe", false, "skip checks against the configured listen address")
	return command
}

func runDoctor(ctx context.Context, out io.Writer, configPath string, explicit bool, options Options, noProbe bool) (bool, error) {
	failed := false
	write := func(format string, args ...any) error {
		_, err := fmt.Fprintf(out, format, args...)
		return err
	}
	status := func(level, format string, args ...any) error {
		if level == "section" {
			return write("\n%s\n", fmt.Sprintf(format, args...))
		}
		if level == "fail" {
			failed = true
		}
		return write("  %-5s %s\n", level, fmt.Sprintf(format, args...))
	}
	executable, err := options.Executable()
	if err != nil {
		executable = "unknown"
	}
	moduleVersion, vcsModified := buildSettings()
	version := currentVersion()
	assets := web.Assets()
	if options.Version != "" {
		version = options.Version
	}
	if options.ModuleVersion != "" {
		moduleVersion = options.ModuleVersion
	}
	if options.VCSModified != "" {
		vcsModified = options.VCSModified
	}
	if options.DashboardAssets != "" {
		assets = options.DashboardAssets
	}
	channel := detectInstallChannel(options.GOOS, executable, version, moduleVersion, vcsModified, assets)
	if err := write("spillway %s at %s (%s)\nServes OpenAI Chat Completions (/v1/chat/completions, /v1/models); the Responses API is not served.\n", version, executable, channel.name); err != nil {
		return failed, err
	}
	if err := write("\nConfig\n"); err != nil {
		return failed, err
	}
	absConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		return failed, err
	}
	pathKind := "-config path"
	if !explicit {
		pathKind = "default path in the current directory"
	}
	info, statErr := os.Stat(configPath)
	if statErr != nil {
		if err := status("fail", "%s (%s): %v", absConfigPath, pathKind, statErr); err != nil {
			return failed, err
		}
	} else {
		if err := status("ok", "%s (%s, %d bytes)", absConfigPath, pathKind, info.Size()); err != nil {
			return failed, err
		}
		linkInfo, linkErr := os.Lstat(configPath)
		if linkErr == nil && linkInfo.Mode()&os.ModeSymlink != 0 {
			target, evalErr := filepath.EvalSymlinks(absConfigPath)
			if evalErr == nil {
				if err := status("info", "symlink resolves to %s", target); err != nil {
					return failed, err
				}
			}
		}
		if options.GOOS == "windows" {
			if err := status("info", "Windows does not enforce file modes; keep this file inside your user profile"); err != nil {
				return failed, err
			}
		} else if info.Mode().Perm()&0o077 != 0 {
			if err := status("warn", "config is readable by group or other users; restrict it to the owner (chmod 600 %s)", absConfigPath); err != nil {
				return failed, err
			}
		}
	}
	lookup := func(name string) (string, bool) {
		if value, ok := options.LookupEnv(name); ok && value != "" {
			return value, true
		}
		return "spillway-doctor-placeholder-" + name, true
	}
	cfg, configErr := config.LoadWithEnv(configPath, lookup)
	if configErr != nil {
		if err := status("fail", "config is invalid: %v", configErr); err != nil {
			return failed, err
		}
		if err := write("\nInstall\n"); err != nil {
			return failed, err
		}
		if err := reportInstall(status, channel, executable, options); err != nil {
			return failed, err
		}
		return failed, nil
	}
	if err := status("ok", "valid with placeholder values for referenced variables"); err != nil {
		return failed, err
	}
	if err := status("info", "providers in this config: %s", strings.Join(sortedProviderNames(cfg.Providers), ", ")); err != nil {
		return failed, err
	}
	if err := reportListen(ctx, status, cfg.Server.Listen, options, noProbe); err != nil {
		return failed, err
	}
	if err := reportVariablesAndKeys(status, configPath, cfg, options); err != nil {
		return failed, err
	}
	if cfg.Store == nil {
		if err := write("\nStore\n"); err != nil {
			return failed, err
		}
		if err := status("ok", "not configured (usage history off)"); err != nil {
			return failed, err
		}
	} else {
		if err := reportStore(ctx, status, cfg.Store.Path); err != nil {
			return failed, err
		}
	}
	if err := write("\nInstall\n"); err != nil {
		return failed, err
	}
	if err := reportInstall(status, channel, executable, options); err != nil {
		return failed, err
	}
	return failed, nil
}

func reportVariablesAndKeys(status func(string, string, ...any) error, path string, cfg config.Config, options Options) error {
	type variableRole struct {
		providers []string
		local     []string
	}
	variables := make(map[string]*variableRole)
	providerVariable := make(map[string]string)
	for name := range cfg.Providers {
		variable, err := config.ProviderKeyVariable(path, name)
		if err != nil {
			return status("fail", "cannot inspect key variable for %s: %v", name, err)
		}
		preset, _ := providers.LookupPreset(name)
		if variable == "" && preset.RequiresKey {
			variable = preset.KeyEnv
		}
		if variable == "" {
			continue
		}
		providerVariable[name] = variable
		if variables[variable] == nil {
			variables[variable] = &variableRole{}
		}
		variables[variable].providers = append(variables[variable].providers, name)
	}
	localSources, err := config.InspectLocalKeySources(path)
	if err != nil {
		return status("fail", "cannot inspect local keys: %v", err)
	}
	for _, source := range localSources {
		if source.Variable != "" {
			if variables[source.Variable] == nil {
				variables[source.Variable] = &variableRole{}
			}
			variables[source.Variable].local = append(variables[source.Variable].local, source.Name)
		}
	}
	if err := writeDoctorSection(status, "Variables"); err != nil {
		return err
	}
	varNames := make([]string, 0, len(variables))
	for name := range variables {
		varNames = append(varNames, name)
	}
	sort.Strings(varNames)
	for _, name := range varNames {
		role := variables[name]
		sort.Strings(role.providers)
		who := ""
		if len(role.providers) == 0 {
			who = "local key"
		} else {
			providers := make([]string, 0, len(role.providers))
			for _, name := range role.providers {
				providers = append(providers, "provider "+name)
			}
			who = strings.Join(providers, ", ")
		}
		if len(role.local) > 0 && len(role.providers) > 0 {
			who += ", local key " + strings.Join(role.local, ", ")
		}
		if value, ok := options.LookupEnv(name); ok && value != "" {
			if err := status("ok", "%s (%s): set in this terminal", name, who); err != nil {
				return err
			}
			continue
		}
		if scope, ok := options.PersistentEnv(name); ok && options.GOOS == "windows" {
			if err := status("fail", "%s (%s): not set in this terminal, but set for your Windows %s; open a new terminal", name, who, scope); err != nil {
				return err
			}
		} else if err := status("fail", "%s (%s): not set in this terminal or persistently", name, who); err != nil {
			return err
		}
		if err := status("info", "set it in this shell with:\n%s", indentLines(environmentRecoveryCommandFor(options.GOOS, options.Shell, name), "        ")); err != nil {
			return err
		}
	}
	if err := writeDoctorSection(status, "Provider keys"); err != nil {
		return err
	}
	for _, name := range sortedProviderNames(cfg.Providers) {
		variable := providerVariable[name]
		if variable == "" {
			continue
		}
		value, ok := options.LookupEnv(variable)
		if !ok || value == "" {
			continue
		}
		if reason := certainKeyMistake(value); reason != "" {
			if err := status("fail", "%s key %s", name, reason); err != nil {
				return err
			}
			continue
		}
		if warning := keyFormatWarning(name, value); warning != "" {
			if err := status("warn", "%s key %s", name, warning); err != nil {
				return err
			}
		}
	}
	if err := writeDoctorSection(status, "Local keys"); err != nil {
		return err
	}
	for i, source := range localSources {
		access := "chat access"
		if source.Dashboard {
			access = "dashboard access"
		}
		declaration := fmt.Sprintf("%s: %s, ", source.Name, access)
		if source.Variable != "" {
			state := "not set in this terminal"
			if value, ok := options.LookupEnv(source.Variable); ok && value != "" {
				state = "set in this terminal"
			}
			declaration += fmt.Sprintf("stored in ${%s} (%s) at line %d", source.Variable, state, source.Line)
		} else {
			declaration += fmt.Sprintf("stored literally in this config at line %d", source.Line)
		}
		if i == 0 || source.Name == "default" {
			declaration += "; client apps use this entry"
		}
		if err := status("ok", "%s", declaration); err != nil {
			return err
		}
		if source.Literal {
			value := cfg.Server.APIKeys[i].Key.Reveal()
			if len(value) < 32 {
				if err := status("warn", "%s local key is shorter than 32 characters", source.Name); err != nil {
					return err
				}
			}
		} else if value, ok := options.LookupEnv(source.Variable); ok && value != "" && len(value) < 32 {
			if err := status("warn", "%s local key is shorter than 32 characters", source.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

func reportStore(ctx context.Context, status func(string, string, ...any) error, path string) error {
	if err := writeDoctorSection(status, "Store"); err != nil {
		return err
	}
	inspection, err := store.Inspect(ctx, path)
	if err != nil {
		return status("fail", "cannot read %s: %v", path, err)
	}
	if inspection.Newer {
		if err := status("fail", "schema %d is newer than supported schema %d", inspection.Version, inspection.Supported); err != nil {
			return err
		}
		return status("info", "run the newer Spillway binary that wrote this database, or point store.path at a new file; keep the existing database and do not delete it")
	}
	return status("ok", "%s schema %d is supported", inspection.Path, inspection.Version)
}

func reportInstall(status func(string, string, ...any) error, channel installChannel, executable string, options Options) error {
	if err := status("ok", "%s; %s", channel.name, channel.instruction); err != nil {
		return err
	}
	path, _ := options.LookupEnv("PATH")
	if resolved, ok := findOnPath("spillway", path, options.GOOS); ok && !sameExecutable(resolved, executable, options.GOOS) {
		return status("warn", "spillway on PATH resolves to %s, not this executable", resolved)
	}
	return nil
}

func writeDoctorSection(status func(string, string, ...any) error, title string) error {
	return status("section", title)
}

func buildSettings() (moduleVersion, vcsModified string) {
	if info, ok := debug.ReadBuildInfo(); ok {
		moduleVersion = info.Main.Version
		for _, setting := range info.Settings {
			if setting.Key == "vcs.modified" {
				vcsModified = setting.Value
			}
		}
	}
	return moduleVersion, vcsModified
}

func indentLines(text, indent string) string {
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = indent + lines[i]
	}
	return strings.Join(lines, "\n")
}

func sameExecutable(left, right, goos string) bool {
	left, _ = filepath.Abs(left)
	right, _ = filepath.Abs(right)
	if goos == "windows" {
		return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

func findOnPath(name, path, goos string) (string, bool) {
	if path == "" {
		return "", false
	}
	for _, directory := range filepath.SplitList(path) {
		candidate := filepath.Join(directory, name)
		if goos == "windows" && filepath.Ext(candidate) == "" {
			candidate += ".exe"
		}
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		absolute, err := filepath.Abs(candidate)
		if err == nil {
			return absolute, true
		}
	}
	return "", false
}
