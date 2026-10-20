package cli

import (
	"io"
	"runtime"
)

// Options injects operating-system and terminal information used by commands.
type Options struct {
	LookupEnv      func(string) (string, bool)
	PersistentEnv  func(string) (string, bool)
	GOOS           string
	StdinTerminal  bool
	StdoutTerminal bool
}

func (o Options) withDefaults(stdin io.Reader, stdout io.Writer) Options {
	if o.LookupEnv == nil {
		o.LookupEnv = defaultLookupEnv
	}
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.PersistentEnv == nil {
		o.PersistentEnv = persistentEnv
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
