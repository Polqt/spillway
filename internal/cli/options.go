package cli

import (
	"io"
	"net/http"
	"runtime"
	"time"
)

// Options injects operating-system and terminal information used by commands.
type Options struct {
	Stdin          io.Reader
	Stdout         io.Writer
	Stderr         io.Writer
	LookupEnv      func(string) (string, bool)
	PersistentEnv  func(string) (string, bool)
	GOOS           string
	Shell          string
	Executable     func() (string, error)
	Now            func() time.Time
	ProbeTransport http.RoundTripper
	StdinTerminal  bool
	StdoutTerminal bool
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

func defaultLookupEnv(name string) (string, bool) {
	return lookupProcessEnv(name)
}
