package config

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/Polqt/spillway/internal/redact"
	"go.yaml.in/yaml/v3"
)

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var providerKeyReference = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`)

func expandConfig(root *yaml.Node, lookupEnv func(string) (string, bool)) error {
	server := mappingValue(root, "server")
	if server != nil {
		if value := mappingValue(server, "listen"); value != nil {
			if err := expandScalar(value, "server.listen", lookupEnv); err != nil {
				return err
			}
		}
		if keys := mappingValue(server, "api_keys"); keys != nil && keys.Kind == yaml.SequenceNode {
			for i, entry := range keys.Content {
				path := fmt.Sprintf("server.api_keys[%d]", i)
				if entry.Kind == yaml.ScalarNode {
					if err := expandScalar(entry, path, lookupEnv); err != nil {
						return err
					}
					continue
				}
				for field, fieldPath := range map[string]string{"name": path + ".name", "key": path + ".key"} {
					if value := mappingValue(entry, field); value != nil {
						if err := expandScalar(value, fieldPath, lookupEnv); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	if logging := mappingValue(root, "logging"); logging != nil {
		if value := mappingValue(logging, "level"); value != nil {
			if err := expandScalar(value, "logging.level", lookupEnv); err != nil {
				return err
			}
		}
	}
	if providers := mappingValue(root, "providers"); providers != nil {
		for i := 0; i+1 < len(providers.Content); i += 2 {
			name := providers.Content[i].Value
			provider := providers.Content[i+1]
			for _, field := range []string{"api_key", "base_url", "timeout"} {
				if value := mappingValue(provider, field); value != nil {
					if err := expandScalar(value, "providers."+name+"."+field, lookupEnv); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func expandScalar(n *yaml.Node, path string, lookupEnv func(string) (string, bool)) error {
	value, err := expandValue(n.Value, path, n.Line, lookupEnv)
	if err != nil {
		return err
	}
	n.Value = value
	return nil
}

func expandValue(value, path string, line int, lookupEnv func(string) (string, bool)) (string, error) {
	var out strings.Builder
	for i := 0; i < len(value); {
		if strings.HasPrefix(value[i:], "$${") {
			out.WriteString("${")
			i += 3
			continue
		}
		if !strings.HasPrefix(value[i:], "${") {
			out.WriteByte(value[i])
			i++
			continue
		}
		end := strings.IndexByte(value[i+2:], '}')
		if end < 0 {
			return "", fmt.Errorf("%s (line %d): unterminated environment variable reference", path, line)
		}
		name := value[i+2 : i+2+end]
		if !environmentName.MatchString(name) {
			return "", fmt.Errorf("%s (line %d): invalid environment variable name", path, line)
		}
		replacement, ok := lookupEnv(name)
		if !ok || replacement == "" {
			return "", fmt.Errorf("%s (line %d): environment variable %s is not set", path, line, name)
		}
		out.WriteString(replacement)
		i += end + 3
	}
	return out.String(), nil
}

func rejectFutureSections(root *yaml.Node) error {
	if root.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := root.Content[i]
		milestone := map[string]string{"router": "v0.3", "budgets": "v0.4", "cache": "v0.4"}[key.Value]
		if milestone != "" {
			return fmt.Errorf("%q is not supported until %s; remove it (line %d)", key.Value, milestone, key.Line)
		}
	}
	return nil
}

func rejectUnknownFields(root *yaml.Node) error {
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("config (line %d): must be a mapping", root.Line)
	}
	if err := rejectFields(root, "config", "server", "logging", "providers", "tiers"); err != nil {
		return err
	}
	server := mappingValue(root, "server")
	if server != nil {
		if server.Kind != yaml.MappingNode && server.Tag != "!!null" {
			return fmt.Errorf("server (line %d): must be a mapping", server.Line)
		}
		if server.Kind == yaml.MappingNode {
			if err := rejectFields(server, "server", "listen", "api_keys"); err != nil {
				return err
			}
			if keys := mappingValue(server, "api_keys"); keys != nil && keys.Kind == yaml.SequenceNode {
				for i, entry := range keys.Content {
					if entry.Kind == yaml.MappingNode {
						if err := rejectFields(entry, fmt.Sprintf("server.api_keys[%d]", i), "name", "key"); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	logging := mappingValue(root, "logging")
	if logging != nil {
		if logging.Kind != yaml.MappingNode && logging.Tag != "!!null" {
			return fmt.Errorf("logging (line %d): must be a mapping", logging.Line)
		}
		if logging.Kind == yaml.MappingNode {
			if err := rejectFields(logging, "logging", "level"); err != nil {
				return err
			}
		}
	}
	providers := mappingValue(root, "providers")
	if providers != nil {
		if providers.Kind != yaml.MappingNode && providers.Tag != "!!null" {
			return fmt.Errorf("providers (line %d): must be a mapping", providers.Line)
		}
		if providers.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(providers.Content); i += 2 {
				key, value := providers.Content[i], providers.Content[i+1]
				if key.Value == "spillway" || key.Value == "auto" {
					return fmt.Errorf("providers.%s is reserved (line %d)", key.Value, key.Line)
				}
				if !slices.Contains(knownProviders, key.Value) {
					return fmt.Errorf("providers: unknown provider; supported: %s", strings.Join(knownProviders, ", "))
				}
				if value.Kind != yaml.MappingNode {
					return fmt.Errorf("providers.%s (line %d): must be a mapping", key.Value, value.Line)
				}
				if err := rejectFields(value, "providers."+key.Value, "api_key", "base_url", "timeout", "free_tier"); err != nil {
					return err
				}
				if freeTier := mappingValue(value, "free_tier"); freeTier != nil && freeTier.Tag != "!!bool" {
					return fmt.Errorf("providers.%s.free_tier (line %d): must be true or false", key.Value, freeTier.Line)
				}
				if freeTier := mappingValue(value, "free_tier"); freeTier != nil && key.Value != "groq" && key.Value != "gemini" {
					return fmt.Errorf("providers.%s.free_tier is only supported for groq and gemini; OpenRouter is checked per model and local providers are always free", key.Value)
				}
				apiKey := mappingValue(value, "api_key")
				if key.Value != "mock" && apiKey != nil && apiKey.Kind == yaml.ScalarNode && apiKey.Tag == "!!str" && !providerKeyReference.MatchString(apiKey.Value) {
					return fmt.Errorf("providers.%s.api_key (line %d): must use an environment reference such as \"${%s_API_KEY}\"", key.Value, apiKey.Line, strings.ToUpper(key.Value))
				}
			}
		}
	}
	tiers := mappingValue(root, "tiers")
	if tiers != nil {
		if tiers.Kind != yaml.MappingNode {
			return fmt.Errorf("tiers (line %d): must be a mapping", tiers.Line)
		}
		for i := 0; i+1 < len(tiers.Content); i += 2 {
			name, tier := tiers.Content[i], tiers.Content[i+1]
			path := "tiers." + name.Value
			if tier.Kind != yaml.MappingNode {
				return fmt.Errorf("%s.models (line %d): use mapping form with a models: field and optional allow_spend field", path, tier.Line)
			}
			if err := rejectFields(tier, path, "models", "allow_spend"); err != nil {
				return err
			}
			if spend := mappingValue(tier, "allow_spend"); spend != nil && spend.Tag != "!!bool" {
				return fmt.Errorf("%s.allow_spend (line %d): must be true or false", path, spend.Line)
			}
			if models := mappingValue(tier, "models"); models != nil && models.Kind != yaml.SequenceNode {
				return fmt.Errorf("%s.models (line %d): must be a list", path, models.Line)
			}
		}
	}
	return nil
}

func rejectFields(node *yaml.Node, path string, allowed ...string) error {
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i]
		if !slices.Contains(allowed, key.Value) {
			return fmt.Errorf("%s (line %d): unknown field", path, key.Line)
		}
	}
	return nil
}

func secretsFromNode(root *yaml.Node) []redact.Secret {
	var secrets []redact.Secret
	server := mappingValue(root, "server")
	if keys := mappingValue(server, "api_keys"); keys != nil && keys.Kind == yaml.SequenceNode {
		for _, entry := range keys.Content {
			if entry.Kind == yaml.ScalarNode && entry.Tag == "!!str" {
				secrets = append(secrets, redact.NewSecret(entry.Value))
				continue
			}
			if value := mappingValue(entry, "key"); value != nil && value.Kind == yaml.ScalarNode && value.Tag == "!!str" {
				secrets = append(secrets, redact.NewSecret(value.Value))
			}
		}
	}
	if providers := mappingValue(root, "providers"); providers != nil && providers.Kind == yaml.MappingNode {
		for i := 1; i < len(providers.Content); i += 2 {
			if value := mappingValue(providers.Content[i], "api_key"); value != nil && value.Kind == yaml.ScalarNode && value.Tag == "!!str" {
				secrets = append(secrets, redact.NewSecret(value.Value))
			}
		}
	}
	return secrets
}

func mappingValue(mapping *yaml.Node, name string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == name {
			return mapping.Content[i+1]
		}
	}
	return nil
}
