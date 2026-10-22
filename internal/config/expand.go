package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/redact"
	"go.yaml.in/yaml/v3"
)

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var providerKeyReference = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`)

// MissingEnvironmentError identifies an unset environment reference in a config file.
type MissingEnvironmentError struct {
	Field    string // Field is the config path containing the unset reference.
	Line     int    // Line is the source line containing the unset reference.
	Variable string // Variable is the name of the unset environment variable.
}

// Error describes the unset environment reference without exposing a value.
func (e *MissingEnvironmentError) Error() string {
	return fmt.Sprintf("%s (line %d): environment variable %s is not set", e.Field, e.Line, e.Variable)
}

func expandConfig(root *yaml.Node, lookupEnv func(string) (string, bool)) error {
	var expansionErrors []error
	expand := func(value *yaml.Node, path string) {
		if err := expandScalar(value, path, lookupEnv); err != nil {
			expansionErrors = append(expansionErrors, err)
		}
	}
	server := mappingValue(root, "server")
	if server != nil {
		for _, field := range []string{"listen", "shutdown_timeout"} {
			if value := mappingValue(server, field); value != nil {
				expand(value, "server."+field)
			}
		}
		if keys := mappingValue(server, "api_keys"); keys != nil && keys.Kind == yaml.SequenceNode {
			for i, entry := range keys.Content {
				path := fmt.Sprintf("server.api_keys[%d]", i)
				if entry.Kind == yaml.ScalarNode {
					expand(entry, path)
					continue
				}
				for _, field := range []struct{ name, path string }{{"name", path + ".name"}, {"key", path + ".key"}} {
					if value := mappingValue(entry, field.name); value != nil {
						expand(value, field.path)
					}
				}
			}
		}
	}
	if logging := mappingValue(root, "logging"); logging != nil {
		if value := mappingValue(logging, "level"); value != nil {
			expand(value, "logging.level")
		}
	}
	if providers := mappingValue(root, "providers"); providers != nil {
		for i := 0; i+1 < len(providers.Content); i += 2 {
			name := providers.Content[i].Value
			provider := providers.Content[i+1]
			for _, field := range []string{"api_key", "base_url", "timeout"} {
				if value := mappingValue(provider, field); value != nil {
					expand(value, "providers."+name+"."+field)
				}
			}
		}
	}
	if store := mappingValue(root, "store"); store != nil {
		for _, field := range []string{"path", "retention"} {
			if value := mappingValue(store, field); value != nil {
				expand(value, "store."+field)
			}
		}
	}
	return errors.Join(expansionErrors...)
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
	var missing []error
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
			missing = append(missing, &MissingEnvironmentError{Field: path, Line: line, Variable: name})
			i += end + 3
			continue
		}
		out.WriteString(replacement)
		i += end + 3
	}
	return out.String(), errors.Join(missing...)
}

func rejectUnknownFields(root *yaml.Node) error {
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("config (line %d): must be a mapping", root.Line)
	}
	if err := rejectFields(root, "config", "server", "logging", "providers", "tiers", "models", "router", "store", "pricing", "budgets", "cache"); err != nil {
		return err
	}
	server := mappingValue(root, "server")
	if server != nil {
		if server.Kind != yaml.MappingNode && server.Tag != "!!null" {
			return fmt.Errorf("server (line %d): must be a mapping", server.Line)
		}
		if server.Kind == yaml.MappingNode {
			if err := rejectFields(server, "server", "listen", "api_keys", "shutdown_timeout"); err != nil {
				return err
			}
			if keys := mappingValue(server, "api_keys"); keys != nil && keys.Kind == yaml.SequenceNode {
				for i, entry := range keys.Content {
					if entry.Kind == yaml.MappingNode {
						if err := rejectFields(entry, fmt.Sprintf("server.api_keys[%d]", i), "name", "key", "dashboard"); err != nil {
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
	providerConfig := mappingValue(root, "providers")
	if providerConfig != nil {
		if providerConfig.Kind != yaml.MappingNode && providerConfig.Tag != "!!null" {
			return fmt.Errorf("providers (line %d): must be a mapping", providerConfig.Line)
		}
		if providerConfig.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(providerConfig.Content); i += 2 {
				key, value := providerConfig.Content[i], providerConfig.Content[i+1]
				if key.Value == "spillway" || key.Value == "auto" {
					return fmt.Errorf("providers.%s is reserved (line %d)", key.Value, key.Line)
				}
				_, legacyName := providers.LookupPreset(key.Value)
				if !legacyName && !validProviderInstanceName(key.Value) {
					return errors.New("providers: instance name must use a safe instance alias matching [a-z][a-z0-9-]{0,31]")
				}
				if value.Kind != yaml.MappingNode {
					return fmt.Errorf("providers.%s (line %d): must be a mapping", key.Value, value.Line)
				}
				serviceType := key.Value
				typeNode := mappingValue(value, "type")
				if typeNode != nil {
					if typeNode.Kind != yaml.ScalarNode || typeNode.Tag != "!!str" || typeNode.Value == "" {
						return fmt.Errorf("providers.%s.type (line %d): must be a provider service type", key.Value, typeNode.Line)
					}
					serviceType = typeNode.Value
				} else if !legacyName {
					return fmt.Errorf("providers: unknown provider; new provider instance names require an explicit type; supported: %s", strings.Join(knownProviders(), ", "))
				}
				preset, known := providers.LookupPreset(serviceType)
				if !known {
					return fmt.Errorf("providers.%s.type: unsupported provider service type; supported: %s", key.Value, strings.Join(knownProviders(), ", "))
				}
				if err := rejectFields(value, "providers."+key.Value, "type", "api_key", "base_url", "timeout", "free_tier", "quota_domains", "budget"); err != nil {
					return err
				}
				if freeTier := mappingValue(value, "free_tier"); freeTier != nil && freeTier.Tag != "!!bool" {
					return fmt.Errorf("providers.%s.free_tier (line %d): must be true or false", key.Value, freeTier.Line)
				}
				if freeTier := mappingValue(value, "free_tier"); freeTier != nil && !preset.FreeTierAttestable {
					return fmt.Errorf("providers.%s.free_tier is only supported for groq and gemini; OpenRouter is checked per model and local providers are always free", key.Value)
				}
				apiKey := mappingValue(value, "api_key")
				if preset.AcceptsKey && apiKey != nil && apiKey.Kind == yaml.ScalarNode && apiKey.Tag == "!!str" && !providerKeyReference.MatchString(apiKey.Value) {
					return fmt.Errorf("providers.%s.api_key (line %d): must use an environment reference such as \"${%s}\"", key.Value, apiKey.Line, preset.KeyEnv)
				}
				if domains := mappingValue(value, "quota_domains"); domains != nil && domains.Kind != yaml.MappingNode {
					return fmt.Errorf("providers.%s.quota_domains (line %d): must be a mapping", key.Value, domains.Line)
				}
				if budget := mappingValue(value, "budget"); budget != nil {
					if err := checkBudgetsShape(budget); err != nil {
						return fmt.Errorf("providers.%s.budget: %w", key.Value, err)
					}
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
	if err := checkModelsShape(mappingValue(root, "models")); err != nil {
		return err
	}
	if err := checkRouterShape(mappingValue(root, "router")); err != nil {
		return err
	}
	if err := checkStoreShape(mappingValue(root, "store")); err != nil {
		return err
	}
	if err := checkPricingShape(mappingValue(root, "pricing")); err != nil {
		return err
	}
	if err := checkBudgetsShape(mappingValue(root, "budgets")); err != nil {
		return err
	}
	return checkCacheShape(mappingValue(root, "cache"))
}

func checkCacheShape(cache *yaml.Node) error {
	if cache == nil {
		return nil
	}
	if cache.Kind != yaml.MappingNode {
		return fmt.Errorf("cache (line %d): must be a mapping", cache.Line)
	}
	for i := 0; i+1 < len(cache.Content); i += 2 {
		key, value := cache.Content[i], cache.Content[i+1]
		if key.Value == "semantic" {
			return errors.New("cache.semantic is not supported")
		}
		if !slices.Contains([]string{"exact", "ttl", "max_mb"}, key.Value) {
			return fmt.Errorf("cache: unknown field %q", key.Value)
		}
		valid := value.Kind == yaml.ScalarNode
		switch key.Value {
		case "exact":
			valid = valid && value.Tag == "!!bool"
		case "ttl":
			valid = valid && value.Tag == "!!str"
		case "max_mb":
			valid = valid && value.Tag == "!!int"
		}
		if !valid {
			return fmt.Errorf("cache.%s (line %d): has an invalid value", key.Value, value.Line)
		}
	}
	return nil
}

func checkBudgetsShape(budgets *yaml.Node) error {
	if budgets == nil {
		return nil
	}
	if budgets.Kind != yaml.MappingNode {
		return fmt.Errorf("budgets (line %d): must be a mapping", budgets.Line)
	}
	for i := 0; i+1 < len(budgets.Content); i += 2 {
		key, value := budgets.Content[i], budgets.Content[i+1]
		if key.Value == "on_exceed" {
			return errors.New("removed: budgets always stop paid models and never free ones (ADR 0010)")
		}
		if key.Value != "daily_usd" && key.Value != "monthly_usd" {
			return fmt.Errorf("budgets (line %d): unknown field %q", key.Line, key.Value)
		}
		if value.Kind != yaml.ScalarNode || (value.Tag != "!!int" && value.Tag != "!!float" && value.Tag != "!!str") {
			return fmt.Errorf("budgets.%s (line %d): must be a decimal number", key.Value, value.Line)
		}
	}
	return nil
}

func checkPricingShape(pricing *yaml.Node) error {
	if pricing == nil {
		return nil
	}
	if pricing.Kind != yaml.MappingNode {
		return fmt.Errorf("pricing (line %d): must be a mapping", pricing.Line)
	}
	for i := 0; i+1 < len(pricing.Content); i += 2 {
		ref, entry := pricing.Content[i], pricing.Content[i+1]
		path := "pricing." + ref.Value
		if entry.Kind != yaml.MappingNode {
			return fmt.Errorf("%s (line %d): must be a mapping", path, entry.Line)
		}
		if err := rejectFields(entry, path, "input_usd_per_mtok", "output_usd_per_mtok"); err != nil {
			return err
		}
		for _, name := range []string{"input_usd_per_mtok", "output_usd_per_mtok"} {
			value := mappingValue(entry, name)
			if value == nil {
				return fmt.Errorf("%s.%s is required; set both input_usd_per_mtok and output_usd_per_mtok", path, name)
			}
			if value.Kind != yaml.ScalarNode || (value.Tag != "!!int" && value.Tag != "!!float" && value.Tag != "!!str") {
				return fmt.Errorf("%s.%s (line %d): must be a decimal number", path, name, value.Line)
			}
		}
	}
	return nil
}

func checkStoreShape(store *yaml.Node) error {
	if store == nil {
		return nil
	}
	if store.Kind != yaml.MappingNode {
		return fmt.Errorf("store (line %d): must be a mapping", store.Line)
	}
	if err := rejectFields(store, "store", "path", "retention"); err != nil {
		return err
	}
	for _, field := range []string{"path", "retention"} {
		if value := mappingValue(store, field); value != nil && (value.Kind != yaml.ScalarNode || value.Tag != "!!str") {
			return fmt.Errorf("store.%s (line %d): must be a string", field, value.Line)
		}
	}
	return nil
}

// maxContextWindow bounds a declared context window to a plausible size.
const maxContextWindow = 10_000_000

// checkModelsShape catches what decoding cannot: a missing capabilities list,
// which must be told apart from an empty one, and an explicit zero window.
func checkModelsShape(models *yaml.Node) error {
	if models == nil {
		return nil
	}
	if models.Kind != yaml.MappingNode {
		return fmt.Errorf("models (line %d): must be a mapping of model references to declarations", models.Line)
	}
	for i := 0; i+1 < len(models.Content); i += 2 {
		ref, declaration := models.Content[i], models.Content[i+1]
		path := "models." + ref.Value
		if declaration.Kind != yaml.MappingNode {
			return fmt.Errorf("%s (line %d): must be a mapping with a capabilities field", path, declaration.Line)
		}
		if err := rejectFields(declaration, path, "capabilities", "context_window"); err != nil {
			return err
		}
		capabilities := mappingValue(declaration, "capabilities")
		if capabilities == nil {
			return fmt.Errorf("%s.capabilities is required; list what the model supports, or [] for text only", path)
		}
		if capabilities.Kind != yaml.SequenceNode {
			return fmt.Errorf("%s.capabilities (line %d): must be a list", path, capabilities.Line)
		}
		if window := mappingValue(declaration, "context_window"); window != nil {
			n, err := strconv.Atoi(window.Value)
			if window.Tag != "!!int" || err != nil || n < 1 || n > maxContextWindow {
				return fmt.Errorf("%s.context_window (line %d): must be a whole number from 1 to %d", path, window.Line, maxContextWindow)
			}
		}
	}
	return nil
}

// routerMilestone names router keys from CONTEXT section 7 that belong to
// later milestones, so they get guidance instead of "unknown field".
var routerMilestone = map[string]string{"judge_model": "v0.5", "confidence_threshold": "v0.5"}

func checkRouterShape(router *yaml.Node) error {
	if router == nil {
		return nil
	}
	if router.Kind != yaml.MappingNode {
		return fmt.Errorf("router (line %d): must be a mapping with an auto field", router.Line)
	}
	for i := 0; i+1 < len(router.Content); i += 2 {
		key := router.Content[i]
		if milestone, later := routerMilestone[key.Value]; later {
			return fmt.Errorf("router.%s is not supported until %s; remove it (line %d)", key.Value, milestone, key.Line)
		}
	}
	if err := rejectFields(router, "router", "auto", "mode", "difficulty", "rules", "classifier"); err != nil {
		return err
	}
	if classifier := mappingValue(router, "classifier"); classifier != nil {
		if classifier.Kind != yaml.MappingNode {
			return fmt.Errorf("router.classifier (line %d): must be a mapping", classifier.Line)
		}
		if err := rejectFields(classifier, "router.classifier", "model"); err != nil {
			return err
		}
		model := mappingValue(classifier, "model")
		if model == nil || model.Kind != yaml.ScalarNode || model.Tag != "!!str" || model.Value == "" {
			return errors.New("router.classifier.model is required; set a pinned ollama model reference")
		}
	}
	auto := mappingValue(router, "auto")
	if auto == nil {
		return errors.New("router.auto is required; list the tiers auto tries, zero-spend tiers first")
	}
	if auto.Kind != yaml.SequenceNode {
		return fmt.Errorf("router.auto (line %d): must be a list of tier names", auto.Line)
	}
	if len(auto.Content) == 0 {
		return errors.New("router.auto must list at least one tier; zero-spend tiers come first")
	}
	mode := mappingValue(router, "mode")
	if mode != nil && (mode.Kind != yaml.ScalarNode || mode.Tag != "!!str" || mode.Value != "rules") {
		return fmt.Errorf("router.mode (line %d): must be rules", mode.Line)
	}
	if mode == nil && (mappingValue(router, "difficulty") != nil || mappingValue(router, "rules") != nil || mappingValue(router, "classifier") != nil) {
		return errors.New("router.difficulty, router.rules and router.classifier require router.mode: rules")
	}
	if err := checkDifficultyShape(mappingValue(router, "difficulty"), mappingValue(router, "rules")); err != nil {
		return err
	}
	return nil
}

func checkDifficultyShape(difficulty, rules *yaml.Node) error {
	if difficulty != nil {
		if difficulty.Kind != yaml.MappingNode {
			return fmt.Errorf("router.difficulty (line %d): must be a mapping", difficulty.Line)
		}
		if err := rejectFields(difficulty, "router.difficulty", "simple", "moderate", "hard"); err != nil {
			return err
		}
		for i := 0; i+1 < len(difficulty.Content); i += 2 {
			key, value := difficulty.Content[i], difficulty.Content[i+1]
			if value.Kind != yaml.SequenceNode || len(value.Content) == 0 {
				return fmt.Errorf("router.difficulty.%s (line %d): must be a non-empty list of tiers", key.Value, value.Line)
			}
		}
	}
	if rules != nil {
		if rules.Kind != yaml.SequenceNode || len(rules.Content) > maxDifficultyRules {
			return fmt.Errorf("router.rules (line %d): must be a list with at most %d rules", rules.Line, maxDifficultyRules)
		}
		for i, rule := range rules.Content {
			path := fmt.Sprintf("router.rules[%d]", i)
			if rule.Kind != yaml.MappingNode {
				return fmt.Errorf("%s (line %d): must be a mapping", path, rule.Line)
			}
			if err := rejectFields(rule, path, "when", "difficulty"); err != nil {
				return err
			}
			when := mappingValue(rule, "when")
			if when == nil || when.Kind != yaml.MappingNode {
				return fmt.Errorf("%s.when (line %d): must be a mapping", path, rule.Line)
			}
			if err := rejectFields(when, path+".when", "estimated_input_tokens_gte", "requested_output_tokens_gte", "tools_present"); err != nil {
				return err
			}
			if len(when.Content) == 0 {
				return fmt.Errorf("%s.when must specify at least one condition", path)
			}
			for j := 0; j+1 < len(when.Content); j += 2 {
				key, value := when.Content[j], when.Content[j+1]
				if key.Value == "tools_present" {
					if value.Kind != yaml.ScalarNode || value.Tag != "!!bool" {
						return fmt.Errorf("%s.when.tools_present (line %d): must be true or false", path, value.Line)
					}
				} else if value.Kind != yaml.ScalarNode || value.Tag != "!!int" {
					return fmt.Errorf("%s.when.%s (line %d): must be a positive bounded integer", path, key.Value, value.Line)
				} else {
					limit := maxDifficultyTokenThreshold
					if value.Value == "" || strings.HasPrefix(value.Value, "-") {
						return fmt.Errorf("%s.when.%s (line %d): must be a positive bounded integer", path, key.Value, value.Line)
					}
					n, err := strconv.Atoi(value.Value)
					if err != nil || n < 1 || n > limit {
						return fmt.Errorf("%s.when.%s (line %d): must be from 1 to %d", path, key.Value, value.Line, limit)
					}
				}
			}
			difficulty := mappingValue(rule, "difficulty")
			if difficulty == nil || difficulty.Kind != yaml.ScalarNode || difficulty.Tag != "!!str" {
				return fmt.Errorf("%s.difficulty: must be simple, moderate or hard", path)
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

func providerKeyEnvironmentNames(root *yaml.Node) map[string]string {
	result := make(map[string]string)
	providersNode := mappingValue(root, "providers")
	if providersNode == nil || providersNode.Kind != yaml.MappingNode {
		return result
	}
	for i := 0; i+1 < len(providersNode.Content); i += 2 {
		name, entry := providersNode.Content[i].Value, providersNode.Content[i+1]
		value := mappingValue(entry, "api_key")
		if value == nil || value.Kind != yaml.ScalarNode || !providerKeyReference.MatchString(value.Value) {
			continue
		}
		result[name] = strings.TrimSuffix(strings.TrimPrefix(value.Value, "${"), "}")
	}
	return result
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
