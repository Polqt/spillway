package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// localKeyName keeps client_app values comparable without case folding and
// short enough not to flood a log line.
var localKeyName = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// knownProviders lists the provider names this release can build.
var knownProviders = []string{"gemini", "groq", "mock", "ollama", "openrouter"}

func (c Config) validate() error {
	if err := validateListen(c.Server.Listen); err != nil {
		return err
	}
	if err := validateLocalKeys(c.Server.APIKeys); err != nil {
		return err
	}
	if !slices.Contains([]string{"debug", "info", "warn", "error"}, c.Logging.Level) {
		return fmt.Errorf("logging.level %q must be debug, info, warn or error", c.Logging.Level)
	}
	return validateProviders(c.Providers)
}

func validateListen(listen string) error {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("server.listen must be host:port, such as 127.0.0.1:8080")
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("server.listen port must be a number from 0 to 65535")
	}
	if host != "" && net.ParseIP(strings.Split(host, "%")[0]) == nil && !validHostname(host) {
		return errors.New("server.listen host must be an IP address or a valid hostname")
	}
	return nil
}

func validHostname(host string) bool {
	if len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

// validateLocalKeys enforces ADR 0003 (a local key on every listen address)
// and that each key resolves to exactly one client app.
func validateLocalKeys(keys LocalKeys) error {
	if len(keys) == 0 {
		return errors.New(`server.api_keys must list at least one local key, even on loopback (ADR 0003); add a long random string as a local key`)
	}
	nameAt := make(map[string]int, len(keys))
	keyAt := make(map[string]int, len(keys))
	for i, k := range keys {
		path := localKeyPath(i)
		if !localKeyName.MatchString(k.Name) {
			return fmt.Errorf("%s.name %q must be 1 to 64 characters of a-z, 0-9 and -", path, k.Name)
		}
		if j, dup := nameAt[k.Name]; dup {
			return fmt.Errorf("%s.name %q duplicates server.api_keys[%d]; give each local key its own name", path, k.Name, j)
		}
		nameAt[k.Name] = i

		v := k.Key.Reveal()
		if v == "" {
			return fmt.Errorf("%s.key must not be empty", path)
		}
		if strings.ContainsFunc(v, unicode.IsSpace) {
			return fmt.Errorf("%s.key must not contain whitespace", path)
		}
		if j, dup := keyAt[v]; dup {
			return fmt.Errorf("%s.key duplicates server.api_keys[%d].key; give each client app its own local key", path, j)
		}
		keyAt[v] = i
	}
	return nil
}

func validateProviders(ps map[string]Provider) error {
	if len(ps) == 0 {
		return errors.New(`providers must configure at least one provider; add "mock: {}" to try Spillway without provider keys`)
	}
	names := make([]string, 0, len(ps))
	for name := range ps {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if name == "spillway" || name == "auto" {
			return fmt.Errorf("providers.%s is reserved; use a provider name: %s", name, strings.Join(knownProviders, ", "))
		}
		if !slices.Contains(knownProviders, name) {
			return fmt.Errorf("providers.%s: unknown provider; supported: %s", name, strings.Join(knownProviders, ", "))
		}
		p := ps[name]
		requiresKey := name == "gemini" || name == "groq" || name == "openrouter"
		if requiresKey && p.APIKey.IsZero() {
			return fmt.Errorf("providers.%s.api_key is required; set it to an environment reference such as \"${%s_API_KEY}\"", name, strings.ToUpper(name))
		}
		if name == "ollama" && !p.APIKey.IsZero() {
			return errors.New("providers.ollama.api_key is not supported; remove it")
		}
		if name == "mock" && !p.APIKey.IsZero() {
			return errors.New("providers.mock.api_key is not supported; remove it")
		}
		if p.Timeout <= 0 || p.Timeout > 30*time.Minute {
			return fmt.Errorf("providers.%s.timeout must be positive and at most 30m", name)
		}
		if p.BaseURL != "" {
			if err := validateBaseURL(name, p.BaseURL); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateBaseURL(provider, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return fmt.Errorf("providers.%s.base_url must be an absolute URL without userinfo, query or fragment", provider)
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !isLoopbackHost(u.Hostname())) {
		return fmt.Errorf("providers.%s.base_url must use https unless the host is loopback", provider)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("providers.%s.base_url scheme must be http or https", provider)
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
