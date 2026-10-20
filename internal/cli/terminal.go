package cli

import (
	"os"

	"github.com/mattn/go-isatty"
)

func isTerminal(value any) bool {
	file, ok := value.(*os.File)
	return ok && (isatty.IsTerminal(file.Fd()) || isatty.IsCygwinTerminal(file.Fd()))
}

func lookupProcessEnv(name string) (string, bool) { return os.LookupEnv(name) }

func defaultExecutable() (string, error) { return os.Executable() }
