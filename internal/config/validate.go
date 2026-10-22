package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Polqt/spillway/internal/providers"
)

// configName keeps identifiers comparable without case folding and short
// enough to use in paths and log lines.
var configName = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
var providerInstanceName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
var numericIdentitySegment = regexp.MustCompile(`(^|-)[0-9]{8,}(-|$)`)

func knownProviders() []string {
	presets := providers.Presets()
	names := make([]string, len(presets))
	for i, preset := range presets {
		names[i] = preset.Name
	}
	return names
}

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
	if err := validateProviders(c.Providers); err != nil {
		return err
	}
	if err := validateTiers(c.Tiers, c.Providers); err != nil {
		return err
	}
	if err := validateModels(c.Models, c.Tiers); err != nil {
		return err
	}
	if err := validateRouter(c.Router, c.Tiers); err != nil {
		return err
	}
	if err := validateRouteProviderUniqueness(c.Router, c.Tiers, c.Providers); err != nil {
		return err
	}
	if err := validateClassifier(c.Router, c.Providers); err != nil {
		return err
	}
	if err := validateStore(c.Store); err != nil {
		return err
	}
	if err := validatePricing(c.Pricing, c.Providers); err != nil {
		return err
	}
	if c.Cache != nil && (c.Cache.TTL < time.Second || c.Cache.TTL > 24*time.Hour || c.Cache.MaxMB < 1 || c.Cache.MaxMB > 1024) {
		return errors.New("cache.ttl must be 1s to 24h and cache.max_mb must be 1 to 1024")
	}
	return validateBudgets(c)
}

// validateRouteProviderUniqueness prevents a virtual route from using two
// separately named instances of one provider service as automatic fallback.
func validateRouteProviderUniqueness(routerConfig Router, tiers map[string]Tier, configured map[string]Provider) error {
	check := func(route string, tierNames []string) error {
		firstInstance := make(map[string]string)
		for _, tierName := range tierNames {
			for _, model := range tiers[tierName].Models {
				ref, err := providers.ParseModelRef(model)
				if err != nil {
					continue // The regular route validator reports malformed references.
				}
				provider, exists := configured[ref.Provider]
				if !exists {
					continue
				}
				serviceType := provider.Type
				if serviceType == "" {
					serviceType = ref.Provider
				}
				if first, exists := firstInstance[serviceType]; exists && first != ref.Provider {
					return fmt.Errorf("%s includes provider instances %s and %s of service type %s; virtual routes may spill only across distinct provider services", route, first, ref.Provider, serviceType)
				}
				firstInstance[serviceType] = ref.Provider
			}
		}
		return nil
	}

	tierNames := make([]string, 0, len(tiers))
	for name := range tiers {
		tierNames = append(tierNames, name)
	}
	slices.Sort(tierNames)
	for _, name := range tierNames {
		if err := check("tiers."+name, []string{name}); err != nil {
			return err
		}
	}
	if len(routerConfig.Auto) > 0 {
		if err := check("router.auto", routerConfig.Auto); err != nil {
			return err
		}
	}
	for difficulty, tierNames := range routerConfig.Difficulty {
		if err := check("router.difficulty."+difficulty, tierNames); err != nil {
			return err
		}
	}
	return nil
}

func validateBudgets(c Config) error {
	instanceBudgets := make([]string, 0)
	for name, provider := range c.Providers {
		if provider.Budget != nil {
			instanceBudgets = append(instanceBudgets, name)
		}
	}
	slices.Sort(instanceBudgets)
	if c.Budgets == nil && len(instanceBudgets) == 0 {
		return nil
	}
	if c.Store == nil {
		if c.Budgets == nil {
			return fmt.Errorf("providers.%s.budget needs store.path so spend survives restarts", instanceBudgets[0])
		}
		return errors.New("budgets need store.path so spend survives restarts")
	}
	if c.Budgets != nil && c.Budgets.DailyMicroUSD <= 0 && c.Budgets.MonthlyMicroUSD <= 0 {
		return errors.New("budgets: set at least one positive daily_usd or monthly_usd limit")
	}
	if c.Budgets != nil && c.Budgets.MonthlyMicroUSD > 0 && c.Store.Retention < 35*24*time.Hour {
		return errors.New("store.retention must be at least 35d with budgets.monthly_usd; increase retention to preserve the full budget period")
	}
	if c.Budgets != nil && c.Budgets.DailyMicroUSD > 0 && c.Store.Retention < 2*24*time.Hour {
		return errors.New("store.retention must be at least 2d with budgets.daily_usd; increase retention to preserve the full budget period")
	}
	for _, name := range instanceBudgets {
		budget := c.Providers[name].Budget
		if budget.MonthlyMicroUSD > 0 && c.Store.Retention < 35*24*time.Hour {
			return fmt.Errorf("store.retention must be at least 35d with providers.%s.budget.monthly_usd; increase retention to preserve the full budget period", name)
		}
		if budget.DailyMicroUSD > 0 && c.Store.Retention < 2*24*time.Hour {
			return fmt.Errorf("store.retention must be at least 2d with providers.%s.budget.daily_usd; increase retention to preserve the full budget period", name)
		}
	}
	for _, tier := range c.Tiers {
		if !tier.AllowSpend {
			continue
		}
		for _, model := range tier.Models {
			ref, _ := providers.ParseModelRef(model)
			if IsZeroSpendEligible(ref, c.Providers[ref.Provider]) {
				continue
			}
			if c.Budgets == nil && c.Providers[ref.Provider].Budget == nil {
				continue
			}
			if _, ok := c.Pricing[model]; !ok {
				return fmt.Errorf("budgets require a price for paid model %s; add pricing.%s", model, model)
			}
		}
	}
	return nil
}

func validatePricing(pricing map[string]Price, configured map[string]Provider) error {
	refs := make([]string, 0, len(pricing))
	for ref := range pricing {
		refs = append(refs, ref)
	}
	slices.Sort(refs)
	for _, value := range refs {
		ref, err := providers.ParseModelRef(value)
		if err != nil || !printableModelRef(value) {
			return fmt.Errorf("pricing.%s: must be a printable provider/model reference", value)
		}
		if _, ok := configured[ref.Provider]; !ok {
			return fmt.Errorf("pricing.%s: provider %s is not configured", value, ref.Provider)
		}
	}
	return nil
}

func validateStore(s *Store) error {
	if s == nil {
		return nil
	}
	if strings.TrimSpace(s.Path) == "" {
		return errors.New("store.path must name a file; set it to a SQLite file path")
	}
	if s.Retention < 24*time.Hour || s.Retention > 3650*24*time.Hour {
		return errors.New("store.retention must be from 1d to 3650d; choose a value in that range")
	}
	cleanPath := filepath.Clean(s.Path)
	if cleanPath == "." || cleanPath == string(filepath.Separator) || filepath.Base(cleanPath) == "." {
		return errors.New("store.path must name a file; choose a file path inside an existing directory")
	}
	if info, err := os.Stat(s.Path); err == nil && info.IsDir() {
		return errors.New("store.path must name a file; choose a file path inside an existing directory")
	}
	parent := filepath.Dir(cleanPath)
	info, err := os.Stat(parent)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("store.path directory must exist; create %s or choose an existing directory", parent)
	}
	return nil
}

// capabilities is the closed set a model declaration may list (ADR 0007).
var capabilities = []string{"tools", "vision", "json_mode", "structured_outputs"}

func validateModels(models map[string]ModelDeclaration, tiers map[string]Tier) error {
	inTier := make(map[string]bool)
	for _, tier := range tiers {
		for _, model := range tier.Models {
			inTier[model] = true
		}
	}
	refs := make([]string, 0, len(models))
	for ref := range models {
		refs = append(refs, ref)
	}
	slices.Sort(refs)
	for _, ref := range refs {
		path := "models." + ref
		if _, err := providers.ParseModelRef(ref); err != nil {
			return fmt.Errorf("%s: model reference must be provider/model", path)
		}
		if !inTier[ref] {
			return fmt.Errorf("%s is not in any tier; capability declarations only affect tier and auto routing, so add it to a tier or remove it", path)
		}
		seen := make(map[string]bool, len(models[ref].Capabilities))
		for i, capability := range models[ref].Capabilities {
			if !slices.Contains(capabilities, capability) {
				return fmt.Errorf("%s.capabilities[%d]: unknown capability %q; use tools, vision, json_mode or structured_outputs", path, i, capability)
			}
			if seen[capability] {
				return fmt.Errorf("%s.capabilities[%d] duplicates an earlier capability", path, i)
			}
			seen[capability] = true
		}
	}
	return nil
}

// maxAutoCandidates keeps auto within the per-request attempt bound of one
// tier (ADR 0008).
const maxAutoCandidates = 16
const maxProviderInstances = 32
const maxConfiguredCandidates = 512
const maxDifficultyRules = 32
const maxDifficultyTokenThreshold = 2_000_000

func validateRouter(router Router, tiers map[string]Tier) error {
	seenTier := make(map[string]bool, len(router.Auto))
	candidates := make(map[string]bool)
	spendAt := -1
	for i, name := range router.Auto {
		tier, ok := tiers[name]
		if !ok {
			return fmt.Errorf("router.auto[%d]: tier %s is not configured", i, name)
		}
		if seenTier[name] {
			return fmt.Errorf("router.auto[%d] duplicates an earlier tier", i)
		}
		seenTier[name] = true
		if tier.AllowSpend && spendAt < 0 {
			spendAt = i
		}
		if !tier.AllowSpend && spendAt >= 0 {
			return fmt.Errorf("router.auto[%d]: tier %s allows spend and must come after every zero-spend tier (ADR 0008)", spendAt, router.Auto[spendAt])
		}
		for _, model := range tier.Models {
			candidates[model] = true
		}
	}
	if len(candidates) > maxAutoCandidates {
		return fmt.Errorf("router.auto resolves to %d candidates; at most %d are allowed, so list fewer tiers or models", len(candidates), maxAutoCandidates)
	}
	if router.Mode == "" {
		if len(router.Difficulty) != 0 || len(router.Rules) != 0 {
			return errors.New("router.difficulty and router.rules require router.mode: rules")
		}
		return nil
	}
	if router.Mode != "rules" {
		return errors.New("router.mode must be rules")
	}
	for _, label := range []string{"simple", "moderate", "hard"} {
		names, ok := router.Difficulty[label]
		if !ok || len(names) == 0 {
			return fmt.Errorf("router.difficulty.%s must list at least one tier", label)
		}
		seen := make(map[string]bool, len(names))
		spendSeen := false
		for i, name := range names {
			if !seenTier[name] {
				return fmt.Errorf("router.difficulty.%s[%d]: tier %s is not listed in router.auto", label, i, name)
			}
			if seen[name] {
				return fmt.Errorf("router.difficulty.%s[%d] duplicates an earlier tier", label, i)
			}
			seen[name] = true
			if tiers[name].AllowSpend {
				spendSeen = true
			} else if spendSeen {
				return fmt.Errorf("router.difficulty.%s[%d]: zero-spend tiers must come before tiers that allow spending", label, i)
			}
		}
	}
	if len(router.Rules) > maxDifficultyRules {
		return errors.New("router.rules must contain at most 32 rules")
	}
	seenRules := make(map[string]int, len(router.Rules))
	for i, rule := range router.Rules {
		if rule.Difficulty != "simple" && rule.Difficulty != "moderate" && rule.Difficulty != "hard" {
			return fmt.Errorf("router.rules[%d].difficulty must be simple, moderate or hard", i)
		}
		when := rule.When
		if when.EstimatedInputTokensGTE == nil && when.RequestedOutputTokensGTE == nil && when.ToolsPresent == nil {
			return fmt.Errorf("router.rules[%d].when must specify at least one condition", i)
		}
		if when.EstimatedInputTokensGTE != nil && (*when.EstimatedInputTokensGTE < 1 || *when.EstimatedInputTokensGTE > maxDifficultyTokenThreshold) {
			return fmt.Errorf("router.rules[%d].when.estimated_input_tokens_gte must be from 1 to 2000000", i)
		}
		if when.RequestedOutputTokensGTE != nil && (*when.RequestedOutputTokensGTE < 1 || *when.RequestedOutputTokensGTE > maxDifficultyTokenThreshold) {
			return fmt.Errorf("router.rules[%d].when.requested_output_tokens_gte must be from 1 to 2000000", i)
		}
		input, output, tools := 0, 0, "unset"
		if when.EstimatedInputTokensGTE != nil {
			input = *when.EstimatedInputTokensGTE
		}
		if when.RequestedOutputTokensGTE != nil {
			output = *when.RequestedOutputTokensGTE
		}
		if when.ToolsPresent != nil {
			tools = fmt.Sprintf("%t", *when.ToolsPresent)
		}
		key := fmt.Sprintf("%d/%d/%s", input, output, tools)
		if earlier, ok := seenRules[key]; ok {
			return fmt.Errorf("router.rules[%d] duplicates conditions from rule %d", i, earlier)
		}
		seenRules[key] = i
	}
	return nil
}

func validateClassifier(router Router, configured map[string]Provider) error {
	if router.Classifier == nil {
		return nil
	}
	if router.Mode != "rules" {
		return errors.New("router.classifier requires router.mode: rules")
	}
	ollama, ok := configured["ollama"]
	if !ok {
		return errors.New("router.classifier requires an explicitly configured providers.ollama section")
	}
	ref, err := providers.ParseModelRef(router.Classifier.Model)
	tagSeparator := strings.LastIndexByte(ref.Model, ':')
	if err != nil || ref.Provider != "ollama" || tagSeparator <= 0 || tagSeparator == len(ref.Model)-1 || strings.HasSuffix(strings.ToLower(ref.Model), ":latest") {
		return errors.New("router.classifier.model must be a pinned ollama model reference with a tag other than latest")
	}
	if ollama.BaseURL != "" {
		parsed, err := url.Parse(ollama.BaseURL)
		if err != nil || !isLoopbackHost(parsed.Hostname()) {
			return errors.New("router.classifier requires providers.ollama.base_url to use a loopback host")
		}
	}
	return nil
}

// printableModelRef reports whether ref can appear in a response header
// unchanged: printable ASCII, no spaces.
func printableModelRef(ref string) bool {
	for i := 0; i < len(ref); i++ {
		if ref[i] <= ' ' || ref[i] > '~' {
			return false
		}
	}
	return true
}

func validateTiers(tiers map[string]Tier, configured map[string]Provider) error {
	names := make([]string, 0, len(tiers))
	candidateRefs := make(map[string]struct{})
	for name := range tiers {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		path := "tiers." + name
		if !configName.MatchString(name) {
			return fmt.Errorf("%s.name must match [a-z0-9-]{1,64}", path)
		}
		if name == "auto" {
			return fmt.Errorf("%s is reserved", path)
		}
		tier := tiers[name]
		if len(tier.Models) < 1 || len(tier.Models) > 16 {
			return fmt.Errorf("%s.models must contain 1 to 16 model references", path)
		}
		seen := make(map[string]struct{}, len(tier.Models))
		for i, model := range tier.Models {
			modelPath := fmt.Sprintf("%s.models[%d]", path, i)
			ref, err := providers.ParseModelRef(model)
			if err != nil {
				return fmt.Errorf("%s: model reference must be provider/model", modelPath)
			}
			if !printableModelRef(model) {
				return fmt.Errorf("%s: model reference must be printable ASCII without spaces", modelPath)
			}
			if _, exists := configured[ref.Provider]; !exists {
				return fmt.Errorf("%s: provider %s is not configured", modelPath, ref.Provider)
			}
			if _, duplicate := seen[model]; duplicate {
				return fmt.Errorf("%s duplicates an earlier model reference", modelPath)
			}
			seen[model] = struct{}{}
			candidateRefs[model] = struct{}{}
			if tier.AllowSpend || IsZeroSpendEligible(ref, configured[ref.Provider]) {
				continue
			}
			fix := fmt.Sprintf("set %s.allow_spend: true", path)
			if ref.Provider == "groq" || ref.Provider == "gemini" {
				fix = fmt.Sprintf("set providers.%s.free_tier: true only if its account or project has no paid plan, or %s", ref.Provider, fix)
			} else {
				fix = "use a local model or OpenRouter :free model, or " + fix
			}
			return fmt.Errorf("%s: %s is not zero-spend eligible: %s (ADR 0005)", modelPath, model, fix)
		}
	}
	if len(candidateRefs) > maxConfiguredCandidates {
		return fmt.Errorf("tiers define %d unique model references; at most %d are allowed", len(candidateRefs), maxConfiguredCandidates)
	}
	return nil
}

// IsZeroSpendEligible reports whether ADR 0005 identifies a model as free from config alone.
func IsZeroSpendEligible(ref providers.ModelRef, provider Provider) bool {
	serviceType := provider.Type
	if serviceType == "" {
		serviceType = ref.Provider
	}
	switch serviceType {
	case "ollama", "mock":
		return true
	case "openrouter":
		return strings.HasSuffix(ref.Model, ":free")
	case "groq", "gemini":
		return provider.FreeTier
	default:
		return false
	}
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
		if !configName.MatchString(k.Name) {
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
	if len(ps) > maxProviderInstances {
		return fmt.Errorf("providers may configure at most %d instances", maxProviderInstances)
	}
	names := make([]string, 0, len(ps))
	for name := range ps {
		names = append(names, name)
	}
	slices.Sort(names)
	credentialEnvAt := make(map[string]map[string]string)
	for _, name := range names {
		if name == "spillway" || name == "auto" {
			return fmt.Errorf("providers.%s is reserved; use a provider name: %s", name, strings.Join(knownProviders(), ", "))
		}
		provider := ps[name]
		preset, legacyName := providers.LookupPreset(name)
		if provider.Type == "" {
			if !legacyName {
				return fmt.Errorf("providers: unknown provider; new provider instance names require an explicit type; supported: %s", strings.Join(knownProviders(), ", "))
			}
		} else {
			servicePreset, supportedType := providers.LookupPreset(provider.Type)
			if !supportedType {
				return fmt.Errorf("providers.%s.type: unsupported provider service type; supported: %s", name, strings.Join(knownProviders(), ", "))
			}
			if legacyName && provider.Type != name {
				return fmt.Errorf("providers.%s.type must match the legacy provider name", name)
			}
			if !legacyName && !validProviderInstanceName(name) {
				return errors.New("providers: instance name must use a safe instance alias matching [a-z][a-z0-9-]{0,31]")
			}
			preset = servicePreset
		}
		if preset.RequiresKey && provider.APIKey.IsZero() {
			return fmt.Errorf("providers.%s.api_key is required; set it to an environment reference such as \"${%s}\"", name, preset.KeyEnv)
		}
		if !preset.AcceptsKey && !provider.APIKey.IsZero() {
			return fmt.Errorf("providers.%s.api_key is not supported; remove it", name)
		}
		if provider.Timeout <= 0 || provider.Timeout > 30*time.Minute {
			return fmt.Errorf("providers.%s.timeout must be positive and at most 30m", name)
		}
		if provider.BaseURL != "" {
			if err := validateBaseURL(name, provider.BaseURL); err != nil {
				return err
			}
		}
		if err := validateQuotaDomains(name, provider); err != nil {
			return err
		}
		if provider.APIKeyEnv != "" {
			if credentialEnvAt[preset.Name] == nil {
				credentialEnvAt[preset.Name] = make(map[string]string)
			}
			if first, exists := credentialEnvAt[preset.Name][provider.APIKeyEnv]; exists && first != name {
				return fmt.Errorf("providers.%s.api_key uses the same environment reference as providers.%s for service %s; give each instance a separate environment reference", name, first, preset.Name)
			}
			credentialEnvAt[preset.Name][provider.APIKeyEnv] = name
		}
	}
	return nil
}

func validProviderInstanceName(name string) bool {
	if !providerInstanceName.MatchString(name) || numericIdentitySegment.MatchString(name) {
		return false
	}
	lower := strings.ToLower(name)
	return !strings.HasPrefix(lower, "sk-") && !strings.HasPrefix(lower, "pk-") && !strings.HasPrefix(lower, "rk-") && !strings.HasPrefix(lower, "aiza-")
}

// ValidProviderInstanceName reports whether name is a safe, non-identifying alias.
func ValidProviderInstanceName(name string) bool { return validProviderInstanceName(name) }

func validateQuotaDomains(name string, provider Provider) error {
	if len(provider.QuotaDomains) > 8 {
		return fmt.Errorf("providers.%s.quota_domains may contain at most 8 scopes", name)
	}
	serviceType := provider.Type
	if serviceType == "" {
		serviceType = name
	}
	scopes := make([]string, 0, len(provider.QuotaDomains))
	for scope := range provider.QuotaDomains {
		scopes = append(scopes, scope)
	}
	slices.Sort(scopes)
	for _, scope := range scopes {
		label := provider.QuotaDomains[scope]
		if !validProviderInstanceName(label) {
			return fmt.Errorf("providers.%s.quota_domains.%s must be a safe alias matching [a-z][a-z0-9-]{0,31}", name, scope)
		}
		if !supportedQuotaScope(serviceType, scope) {
			return fmt.Errorf("providers.%s.quota_domains contains a scope that is not supported for %s", name, serviceType)
		}
	}
	return nil
}

func supportedQuotaScope(serviceType, scope string) bool {
	switch serviceType {
	case "gemini":
		return scope == "project" || scope == "billing_account"
	case "groq":
		return scope == "organization" || scope == "project"
	case "openrouter":
		return scope == "account" || scope == "workspace" || scope == "member" || scope == "key"
	default:
		return false
	}
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
