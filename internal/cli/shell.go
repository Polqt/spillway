package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func environmentRecoveryCommand(name string) string {
	shell, _ := os.LookupEnv("SHELL")
	return environmentRecoveryCommandFor(runtime.GOOS, shell, name)
}

func environmentRecoveryCommandFor(goos, shell, name string) string {
	if goos == "windows" {
		return "$env:" + name + " = [Net.NetworkCredential]::new('', (Read-Host -AsSecureString 'provider key')).Password\n" +
			"[Environment]::SetEnvironmentVariable('" + name + "', $env:" + name + ", 'User')"
	}
	if strings.EqualFold(filepath.Base(shell), "fish") {
		return "read -s " + name + "; and set -gx " + name + " $" + name + "\nset -Ux " + name + " $" + name
	}
	return "read -rs " + name + " && export " + name + "\nUse a secret manager or a private file (chmod 600) sourced by your shell profile to persist it."
}
