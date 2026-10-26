package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"
)

func installCommand(ctx context.Context, options Options) *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "replace the Spillway executable found on PATH",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !options.StdinTerminal {
				return commandFailure{code: 1, err: fmt.Errorf("install requires an interactive terminal")}
			}
			source, err := options.Executable()
			if err != nil {
				return commandFailure{code: 1, err: fmt.Errorf("find current executable: %w", err)}
			}
			path, _ := options.LookupEnv("PATH")
			target, found, err := findOnPath(ctx, "spillway", path, options.GOOS)
			if err != nil {
				return commandFailure{code: 1, err: err}
			}
			if !found {
				return writeCommandResult(ctx, cmd, "spillway install", "No Spillway executable was found on PATH; no changes made.\n", options.interactive(), options.Stdin, cmd.OutOrStdout())
			}
			if sameExecutable(source, target, options.GOOS) {
				return writeCommandResult(ctx, cmd, "spillway install", fmt.Sprintf("This executable is already at %s.\n", target), options.interactive(), options.Stdin, cmd.OutOrStdout())
			}
			prompt := fmt.Sprintf("%s\n\n%s\n%s", brandHeader("install"), warnStyle.Bold(true).Render("Replace the PATH executable?"), mutedStyle.Render(target+"  ←  spillway "+currentVersion()))
			if _, err := fmt.Fprint(styledWriter(cmd.OutOrStdout()), lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorViolet).Padding(1, 2).Render(prompt)+"\n\n[y/N] "); err != nil {
				return commandFailure{code: 1, err: err}
			}
			answer, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				return commandFailure{code: 1, err: fmt.Errorf("read confirmation: %w", err)}
			}
			if !strings.EqualFold(strings.TrimSpace(answer), "y") && !strings.EqualFold(strings.TrimSpace(answer), "yes") {
				return writeCommandResult(ctx, cmd, "spillway install", "No changes made.\n", options.interactive(), options.Stdin, cmd.OutOrStdout())
			}
			if err := replaceExecutable(ctx, source, target); err != nil {
				return commandFailure{code: 1, err: err}
			}
			return writeCommandResult(ctx, cmd, "spillway install", fmt.Sprintf("Installed this version at %s.\n", target), options.interactive(), options.Stdin, cmd.OutOrStdout())
		},
	}
}

func replaceExecutable(ctx context.Context, source, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("inspect current executable: %w", err)
	}
	if !sourceInfo.Mode().IsRegular() {
		return fmt.Errorf("current executable is not a regular file")
	}
	targetInfo, err := os.Lstat(target)
	if err != nil {
		return fmt.Errorf("inspect PATH executable: %w", err)
	}
	if targetInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("PATH executable is a symlink; upgrade it through its package manager")
	}
	if !targetInfo.Mode().IsRegular() {
		return fmt.Errorf("PATH target is not a regular file")
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), ".spillway-install-*")
	if err != nil {
		return fmt.Errorf("prepare replacement beside PATH executable: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	// #nosec G304 -- source is the executable selected by os.Executable.
	input, err := os.Open(source)
	if err != nil {
		_ = temporary.Close()
		return fmt.Errorf("open current executable: %w", err)
	}
	if _, err := io.Copy(temporary, input); err != nil {
		_ = input.Close()
		_ = temporary.Close()
		return fmt.Errorf("copy current executable: %w", err)
	}
	if err := input.Close(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("close current executable: %w", err)
	}
	if err := temporary.Chmod(sourceInfo.Mode().Perm()); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("preserve executable permissions: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync replacement executable: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close replacement executable: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		return fmt.Errorf("replace PATH executable: %w", err)
	}
	return nil
}
