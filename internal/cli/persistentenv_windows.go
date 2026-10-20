//go:build windows

package cli

import "golang.org/x/sys/windows/registry"

func persistentEnv(name string) (string, bool) {
	for _, location := range []struct {
		root  registry.Key
		scope string
	}{{registry.CURRENT_USER, "user"}, {registry.LOCAL_MACHINE, "machine"}} {
		key, err := registry.OpenKey(location.root, `Environment`, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		_, _, err = key.GetValue(name, nil)
		_ = key.Close()
		if err == nil {
			return location.scope, true
		}
	}
	return "", false
}
