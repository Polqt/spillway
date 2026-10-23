package cli

import (
	"io"
	"net/http"
	"runtime"
	"time"
)

// Options injects operating-system and terminal information used by commands.
type Options struct {
	Stdin           io.Reader                   // Stdin is the command input stream.
	Stdout          io.Writer                   // Stdout is the command output stream.
	Stderr          io.Writer                   // Stderr is the command error stream.
	LookupEnv       func(string) (string, bool) // LookupEnv reads an environment variable.
	PersistentEnv   func(string) (string, bool) // PersistentEnv reads a user-level environment variable.
	GOOS            string                      // GOOS names the operating system used for platform-specific behavior.
	Version         string                      // Version is the version embedded in the binary.
	ModuleVersion   string                      // ModuleVersion is the version recorded in Go build information.
	VCSModified     string                      // VCSModified reports whether the source tree was modified at build time.
	DashboardAssets string                      // DashboardAssets identifies the embedded or fallback dashboard.
	Shell           string                      // Shell is the user's shell path, when known.
	Executable      func() (string, error)      // Executable returns the current executable path.
	Now             func() time.Time            // Now returns the current time.
	ProbeTransport  http.RoundTripper           // ProbeTransport sends doctor HTTP probes.
	StdinTerminal   bool                        // StdinTerminal reports whether stdin is an interactive terminal.
	StdoutTerminal  bool                        // StdoutTerminal reports whether stdout is an interactive terminal.
}

func (o Options) withDefaults(stdin io.Reader, stdout, stderr io.Writer) Options {
	if o.Stdin == nil {
		o.Stdin = stdin
	}
	if o.Stdout == nil {
		o.Stdout = stdout
	}
	if o.Stderr == nil {
		o.Stderr = stderr
	}
	if o.LookupEnv == nil {
		o.LookupEnv = defaultLookupEnv
	}
	if o.Shell == "" {
		if value, ok := o.LookupEnv("SHELL"); ok {
			o.Shell = value
		}
	}
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.PersistentEnv == nil {
		o.PersistentEnv = persistentEnv
	}
	if o.Executable == nil {
		o.Executable = defaultExecutable
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if !o.StdinTerminal {
		o.StdinTerminal = isTerminal(stdin)
	}
	if !o.StdoutTerminal {
		o.StdoutTerminal = isTerminal(stdout)
	}
	return o
}

func (o Options) interactive() bool {
	return o.StdinTerminal && o.StdoutTerminal && tuiAllowed(o.LookupEnv)
}

func defaultLookupEnv(name string) (string, bool) {
	return lookupProcessEnv(name)
}
