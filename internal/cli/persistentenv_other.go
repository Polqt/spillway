//go:build !windows

package cli

func persistentEnv(string) (string, bool) { return "", false }
