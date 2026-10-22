package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Polqt/spillway/internal/config"
	"github.com/Polqt/spillway/internal/providers"
	"go.yaml.in/yaml/v3"
)

const (
	localKeyCI     = "sk-test-local-ci"
	localKeyCursor = "sk-test-local-cursor"
)

func TestParse_Valid(t *testing.T) {
	t.Parallel()
	cfg, err := config.Parse([]byte(`
server:
  api_keys:
    - name: demo
      key: sk-test-local-ci
providers:
  mock: {}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Server.Listen != "127.0.0.1:8080" {
		t.Errorf("Listen = %q, want default 127.0.0.1:8080", cfg.Server.Listen)
	}
	if len(cfg.Server.APIKeys) != 1 || cfg.Server.APIKeys[0].Name != "demo" || cfg.Server.APIKeys[0].Key.Reveal() != localKeyCI {
		t.Errorf("APIKeys = %+v, want one key named demo", cfg.Server.APIKeys)
	}
	if _, ok := cfg.Providers["mock"]; !ok || len(cfg.Providers) != 1 {
		t.Errorf("Providers = %v, want only mock", cfg.Providers)
	}
}

func TestParse_ProviderInstanceFields(t *testing.T) {
	t.Parallel()
	input := []byte(`
server:
  api_keys: [sk-test-local-ci]
providers:
  gemini:
    api_key: "${GEMINI_API_KEY}"
  gemini-research:
    type: gemini
    api_key: "${GEMINI_RESEARCH_API_KEY}"
    quota_domains:
      project: research-project
      billing_account: shared-billing
    budget:
      monthly_usd: 12.50
store:
  path: ./usage.db
  retention: 90d
`)
	cfg, err := config.ParseWithEnv(input, func(name string) (string, bool) {
		if name == "GEMINI_API_KEY" {
			return "sk-test-gemini-legacy", true
		}
		return "sk-test-gemini-research", true
	})
	if err != nil {
		t.Fatalf("ParseWithEnv: %v", err)
	}
	legacy := cfg.Providers["gemini"]
	if legacy.Type != "" {
		t.Errorf("legacy type = %q, want empty so preset-name resolution remains implicit", legacy.Type)
	}
	instance := cfg.Providers["gemini-research"]
	if instance.Type != "gemini" {
		t.Errorf("instance type = %q, want gemini", instance.Type)
	}
	if instance.QuotaDomains["project"] != "research-project" || instance.QuotaDomains["billing_account"] != "shared-billing" {
		t.Errorf("quota domains = %v, want project and billing account aliases", instance.QuotaDomains)
	}
	if instance.Budget == nil || instance.Budget.MonthlyMicroUSD != 12_500_000 {
		t.Errorf("instance budget = %+v, want monthly 12.5 USD", instance.Budget)
	}
	var redacted strings.Builder
	if err := cfg.WriteRedacted(&redacted); err != nil {
		t.Fatalf("WriteRedacted: %v", err)
	}
	if strings.Contains(redacted.String(), "sk-test-gemini-legacy") || strings.Contains(redacted.String(), "sk-test-gemini-research") {
		t.Fatalf("redacted output exposed provider credential: %q", redacted.String())
	}
	if !strings.Contains(redacted.String(), "type: gemini") || !strings.Contains(redacted.String(), "monthly_usd: 12.5") {
		t.Fatalf("redacted output omitted instance config fields: %q", redacted.String())
	}
	if strings.Contains(redacted.String(), "research-project") || strings.Contains(redacted.String(), "shared-billing") {
		t.Fatalf("redacted output exposed quota-domain labels: %q", redacted.String())
	}
	if legacy.APIKey.Reveal() == instance.APIKey.Reveal() {
		t.Fatal("instances resolved to the same credential, want independent environment references")
	}
}

func TestParse_ProviderInstanceBudgetNeedsStoreRetention(t *testing.T) {
	t.Parallel()
	base := "server:\n  api_keys: [sk-test-local-ci]\nproviders:\n  gemini-research:\n    type: gemini\n    api_key: \"${GEMINI_API_KEY}\"\n    budget:\n      monthly_usd: 1\n"
	_, err := config.ParseWithEnv([]byte(base), func(string) (string, bool) { return "sk-test-provider", true })
	if err == nil || !strings.Contains(err.Error(), "providers.gemini-research.budget needs store.path") {
		t.Fatalf("ParseWithEnv error = %v, want store requirement", err)
	}
	withShortRetention := base + "store:\n  path: ./usage.db\n  retention: 34d\n"
	_, err = config.ParseWithEnv([]byte(withShortRetention), func(string) (string, bool) { return "sk-test-provider", true })
	if err == nil || !strings.Contains(err.Error(), "store.retention must be at least 35d") {
		t.Fatalf("ParseWithEnv error = %v, want monthly retention requirement", err)
	}
}

func TestParse_ProviderInstanceBudgetRejectsInvalidLimits(t *testing.T) {
	t.Parallel()
	for _, budget := range []string{
		"{}",
		"{daily_usd: 0}",
		"{monthly_usd: -1}",
		"{monthly_usd: 1.0000001}",
		"{monthly_usd: 1e9}",
		"{yearly_usd: 1}",
	} {
		t.Run(budget, func(t *testing.T) {
			t.Parallel()
			input := "server:\n  api_keys: [sk-test-local-ci]\nproviders:\n  gemini-research:\n    type: gemini\n    api_key: \"${GEMINI_API_KEY}\"\n    budget: " + budget + "\n"
			_, err := config.ParseWithEnv([]byte(input), func(string) (string, bool) { return "sk-test-provider", true })
			if err == nil {
				t.Fatalf("ParseWithEnv(%s) succeeded, want invalid budget rejection", budget)
			}
			if strings.Contains(err.Error(), "sk-test-provider") {
				t.Fatalf("budget error exposed provider credential: %v", err)
			}
		})
	}
}

func TestParse_ProviderInstanceFreeTierUsesServiceType(t *testing.T) {
	t.Parallel()
	input := []byte(`
server:
  api_keys: [sk-test-local-ci]
providers:
  gemini-research:
    type: gemini
    api_key: "${GEMINI_API_KEY}"
    free_tier: true
tiers:
  free:
    models: [gemini-research/gemini-model]
`)
	_, err := config.ParseWithEnv(input, func(string) (string, bool) { return "sk-test-provider", true })
	if err != nil {
		t.Fatalf("ParseWithEnv: %v", err)
	}
}

func TestParse_ProviderInstanceQuotaScopesByService(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		service   string
		scopes    string
		keyEnv    string
		wantError string
	}{
		{name: "Gemini project and billing", service: "gemini", scopes: "project: gemini-project\n      billing_account: google-billing", keyEnv: "GEMINI_API_KEY"},
		{name: "Groq organization and project", service: "groq", scopes: "organization: groq-org\n      project: groq-project", keyEnv: "GROQ_API_KEY"},
		{name: "OpenRouter account, workspace, member and key", service: "openrouter", scopes: "account: router-account\n      workspace: router-workspace\n      member: router-member\n      key: router-key", keyEnv: "OPENROUTER_API_KEY"},
		{name: "Ollama has no verified hosted quota scope", service: "ollama", scopes: "project: local-project", wantError: "scope that is not supported for ollama"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			input := fmt.Sprintf("server:\n  api_keys: [sk-test-local-ci]\nproviders:\n  workload-a:\n    type: %s\n    quota_domains:\n      %s\n", tt.service, tt.scopes)
			if tt.keyEnv != "" {
				input = strings.Replace(input, "    quota_domains:", "    api_key: \"${"+tt.keyEnv+"}\"\n    quota_domains:", 1)
			}
			_, err := config.ParseWithEnv([]byte(input), func(string) (string, bool) { return "sk-test-provider", true })
			if tt.wantError == "" && err != nil {
				t.Fatalf("ParseWithEnv: %v", err)
			}
			if tt.wantError != "" && (err == nil || !strings.Contains(err.Error(), tt.wantError)) {
				t.Errorf("ParseWithEnv error = %v, want substring %q", err, tt.wantError)
			}
		})
	}
}

func TestParse_InstanceBudgetAboveProcessBudgetStillParses(t *testing.T) {
	t.Parallel()
	input := []byte(`
server:
  api_keys: [sk-test-local-ci]
providers:
  gemini-research:
    type: gemini
    api_key: "${GEMINI_API_KEY}"
    budget:
      monthly_usd: 10
store:
  path: ./usage.db
  retention: 90d
budgets:
  monthly_usd: 5
`)
	cfg, err := config.ParseWithEnv(input, func(string) (string, bool) { return "sk-test-provider", true })
	if err != nil {
		t.Fatalf("ParseWithEnv: %v", err)
	}
	if cfg.Providers["gemini-research"].Budget.MonthlyMicroUSD <= cfg.Budgets.MonthlyMicroUSD {
		t.Fatal("fixture must place the instance budget above the process budget")
	}
}

func TestParse_SharedQuotaDomainLabelsCanRepeatAcrossInstances(t *testing.T) {
	t.Parallel()
	input := []byte(`
server:
  api_keys: [sk-test-local-ci]
providers:
  gemini-research:
    type: gemini
    api_key: "${GEMINI_RESEARCH_API_KEY}"
    quota_domains: {project: shared-project}
  gemini-staging:
    type: gemini
    api_key: "${GEMINI_STAGING_API_KEY}"
    quota_domains: {project: shared-project}
`)
	cfg, err := config.ParseWithEnv(input, func(name string) (string, bool) {
		return "sk-test-" + strings.ToLower(name), true
	})
	if err != nil {
		t.Fatalf("ParseWithEnv: %v", err)
	}
	if cfg.Providers["gemini-research"].QuotaDomains["project"] != cfg.Providers["gemini-staging"].QuotaDomains["project"] {
		t.Fatal("shared project quota-domain labels did not remain identical")
	}
}

func TestParse_ProviderInstanceNamesAndDomains(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		provider  string
		wantError string
	}{
		{name: "email-like alias", provider: "gemini-user@example.com:\n    type: gemini\n    api_key: \"${GEMINI_API_KEY}\"\n", wantError: "safe instance alias"},
		{name: "email-like scalar provider", provider: "gemini-user@example.com: anything\n", wantError: "safe instance alias"},
		{name: "credential-like alias", provider: "sk-test-secret:\n    type: gemini\n    api_key: \"${GEMINI_API_KEY}\"\n", wantError: "safe instance alias"},
		{name: "underscore alias", provider: "gemini_research:\n    type: gemini\n    api_key: \"${GEMINI_API_KEY}\"\n", wantError: "safe instance alias"},
		{name: "numeric identity segment", provider: "research-12345678:\n    type: gemini\n    api_key: \"${GEMINI_API_KEY}\"\n", wantError: "safe instance alias"},
		{name: "unsupported domain scope", provider: "gemini-research:\n    type: gemini\n    api_key: \"${GEMINI_API_KEY}\"\n    quota_domains:\n      organization: org-a\n", wantError: "quota_domains contains a scope that is not supported for gemini"},
		{name: "duplicate domain scope", provider: "gemini-research:\n    type: gemini\n    api_key: \"${GEMINI_API_KEY}\"\n    quota_domains:\n      project: research-project\n      project: other-project\n", wantError: "duplicate scope"},
		{name: "unsafe domain alias", provider: "gemini-research:\n    type: gemini\n    api_key: \"${GEMINI_API_KEY}\"\n    quota_domains:\n      project: project@example.com\n", wantError: "quota_domains.project must be a safe alias"},
		{name: "missing type for new alias", provider: "gemini-research:\n    api_key: \"${GEMINI_API_KEY}\"\n", wantError: "new provider instance names require an explicit type"},
		{name: "canonical alias type mismatch", provider: "gemini:\n    type: groq\n    api_key: \"${GEMINI_API_KEY}\"\n", wantError: "type must match the legacy provider name"},
		{name: "unknown type", provider: "gemini-research:\n    type: project-secret-id\n    api_key: \"${GEMINI_API_KEY}\"\n", wantError: "unsupported provider service type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data := "server:\n  api_keys: [sk-test-local-ci]\nproviders:\n  " + tt.provider
			_, err := config.ParseWithEnv([]byte(data), func(string) (string, bool) { return "sk-test-provider", true })
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Errorf("ParseWithEnv error = %v, want substring %q", err, tt.wantError)
			}
			if strings.Contains(errString(err), "example.com") || strings.Contains(errString(err), "project-secret-id") {
				t.Errorf("error exposed identity-like input: %v", err)
			}
		})
	}
}

func TestParse_ProviderInstanceRejectsDuplicateAliases(t *testing.T) {
	t.Parallel()
	input := []byte(`
server:
  api_keys: [sk-test-local-ci]
providers:
  gemini-research:
    type: gemini
    api_key: "${GEMINI_RESEARCH_API_KEY}"
  gemini-research:
    type: gemini
    api_key: "${GEMINI_RESEARCH_API_KEY_2}"
`)
	_, err := config.ParseWithEnv(input, func(string) (string, bool) { return "sk-test-provider", true })
	if err == nil {
		t.Fatal("ParseWithEnv succeeded, want duplicate instance alias rejection")
	}
	if strings.Contains(err.Error(), "sk-test-provider") {
		t.Fatalf("duplicate alias error exposed provider credential: %v", err)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestParse_DashboardLocalKey(t *testing.T) {
	t.Parallel()
	cfg, err := config.Parse([]byte("server:\n  api_keys: [{name: admin, key: sk-test-local-ci, dashboard: true}]\nproviders:\n  mock: {}\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !cfg.Server.APIKeys[0].Dashboard {
		t.Fatal("dashboard key flag = false, want true")
	}
	var output strings.Builder
	if err := cfg.WriteRedacted(&output); err != nil {
		t.Fatalf("WriteRedacted: %v", err)
	}
	if !strings.Contains(output.String(), "dashboard: true") {
		t.Errorf("redacted config = %q, want dashboard flag", output.String())
	}
	if strings.Contains(output.String(), localKeyCI) {
		t.Errorf("redacted config exposes local key: %q", output.String())
	}
}

func TestParse_DashboardFlagMustBeBoolean(t *testing.T) {
	t.Parallel()
	_, err := config.Parse([]byte("server:\n  api_keys: [{name: admin, key: sk-test-local-ci, dashboard: yes}]\nproviders:\n  mock: {}\n"))
	if err == nil || !strings.Contains(err.Error(), "server.api_keys[0].dashboard") {
		t.Fatalf("Parse error = %v, want dashboard boolean validation", err)
	}
}

func TestParse_StoreConfig(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "usage.db")
	cfg, err := config.Parse([]byte("server:\n  api_keys: [sk-test-local-ci]\nproviders:\n  mock: {}\nstore:\n  path: " + path + "\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Store.Path != path || cfg.Store.Retention != 90*24*time.Hour {
		t.Errorf("Store = %+v, want path %q and 90 day retention", cfg.Store, path)
	}
}

func TestParse_ShutdownTimeoutBounds(t *testing.T) {
	t.Parallel()
	base := "server:\n  api_keys: [sk-test-local-ci]\nproviders:\n  mock: {}\n"
	cfg, err := config.Parse([]byte(base))
	if err != nil {
		t.Fatalf("Parse default: %v", err)
	}
	if cfg.Server.ShutdownTimeout != 10*time.Second {
		t.Errorf("ShutdownTimeout = %s, want 10s", cfg.Server.ShutdownTimeout)
	}
	for _, value := range []string{"1s", "5m"} {
		t.Run("accept "+value, func(t *testing.T) {
			t.Parallel()
			parsed, err := config.Parse([]byte(strings.Replace(base, "server:\n", "server:\n  shutdown_timeout: "+value+"\n", 1)))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			want, _ := time.ParseDuration(value)
			if parsed.Server.ShutdownTimeout != want {
				t.Errorf("ShutdownTimeout = %s, want %s", parsed.Server.ShutdownTimeout, want)
			}
		})
	}
	for _, value := range []string{"0s", "-1s", "5m1s", "nope"} {
		t.Run("reject "+value, func(t *testing.T) {
			t.Parallel()
			input := strings.Replace(base, "server:\n", "server:\n  shutdown_timeout: "+value+"\n", 1)
			if _, err := config.Parse([]byte(input)); err == nil || !strings.Contains(err.Error(), "server.shutdown_timeout") {
				t.Errorf("Parse error = %v, want server.shutdown_timeout validation", err)
			}
		})
	}
	var output strings.Builder
	if err := cfg.WriteRedacted(&output); err != nil {
		t.Fatalf("WriteRedacted: %v", err)
	}
	if !strings.Contains(output.String(), "shutdown_timeout: 10s") {
		t.Errorf("redacted config = %q, want shutdown timeout", output.String())
	}
}

func TestParse_ExactCacheConfig(t *testing.T) {
	t.Parallel()
	cfg, err := config.Parse([]byte(`server:
  api_keys: [sk-test-local-ci]
providers:
  mock: {}
cache:
  exact: true
  ttl: 1h
  max_mb: 8
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Cache == nil || !cfg.Cache.Exact || cfg.Cache.TTL != time.Hour || cfg.Cache.MaxMB != 8 {
		t.Fatalf("Cache = %+v, want exact enabled, 1h TTL and 8 MiB", cfg.Cache)
	}
}

func TestParse_ExactCacheRejectsUnsupportedConfig(t *testing.T) {
	t.Parallel()
	base := "server:\n  api_keys: [sk-test-local-ci]\nproviders:\n  mock: {}\n"
	for _, tt := range []struct{ name, cache, want string }{
		{"semantic", "semantic: true\n", "cache.semantic is not supported"},
		{"unknown", "unknown: true\n", "cache: unknown field"},
		{"ttl too short", "exact: true\nttl: 999ms\n", "cache.ttl"},
		{"ttl too long", "exact: true\nttl: 25h\n", "cache.ttl"},
		{"max too small", "exact: true\nmax_mb: 0\n", "cache.max_mb"},
		{"max too large", "exact: true\nmax_mb: 1025\n", "cache.max_mb"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Parse([]byte(base + "cache:\n  " + strings.ReplaceAll(strings.TrimSuffix(tt.cache, "\n"), "\n", "\n  ") + "\n"))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestParse_BudgetsValidationAndRedactedOutput(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "usage.db")
	cfg, err := config.Parse([]byte("server:\n  api_keys: [sk-test-local-ci]\nproviders:\n  mock: {}\nstore:\n  path: " + path + "\n  retention: 35d\nbudgets:\n  daily_usd: 0.025001\n  monthly_usd: 10\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Budgets.DailyMicroUSD != 25_001 || cfg.Budgets.MonthlyMicroUSD != 10_000_000 {
		t.Fatalf("Budgets = %+v, want integer micro-USD", cfg.Budgets)
	}
	var output strings.Builder
	if err := cfg.WriteRedacted(&output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "daily_usd: 0.025001") || !strings.Contains(output.String(), "monthly_usd: 10") {
		t.Errorf("check output omitted budgets:\n%s", output.String())
	}
}

func TestParse_BudgetsFailClosedValidation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := "server:\n  api_keys: [sk-test-local-ci]\nproviders:\n  mock: {}\n"
	for _, tt := range []struct{ name, config, want string }{
		{"requires store", "budgets: {daily_usd: 1}\n", "budgets need store.path"},
		{"requires positive limit", "store: {path: '" + filepath.Join(dir, "db") + "'}\nbudgets: {}\n", "at least one positive"},
		{"daily retention", "store: {path: '" + filepath.Join(dir, "db") + "', retention: 1d}\nbudgets: {daily_usd: 1}\n", "at least 2d"},
		{"monthly retention", "store: {path: '" + filepath.Join(dir, "db") + "', retention: 30d}\nbudgets: {monthly_usd: 1}\n", "at least 35d"},
		{"removed on exceed", "store: {path: '" + filepath.Join(dir, "db") + "', retention: 35d}\nbudgets: {daily_usd: 1, on_exceed: block}\n", "removed: budgets always stop paid models and never free ones (ADR 0010)"},
		{"zero limit", "store: {path: '" + filepath.Join(dir, "db") + "'}\nbudgets: {daily_usd: 0}\n", "positive decimal"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Parse([]byte(base + tt.config))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestParse_BudgetRequiresPricesForPaidSpendTier(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	data := "server:\n  api_keys: [sk-test-local-ci]\nproviders:\n  openrouter: {api_key: \"${OPENROUTER_API_KEY}\"}\nstore: {path: '" + filepath.Join(dir, "usage.db") + "', retention: 35d}\nbudgets: {monthly_usd: 5}\ntiers:\n  paid:\n    allow_spend: true\n    models: [openrouter/vendor/model]\n"
	_, err := config.ParseWithEnv([]byte(data), func(name string) (string, bool) {
		return map[string]string{"OPENROUTER_API_KEY": "sk-test-openrouter"}[name], name == "OPENROUTER_API_KEY"
	})
	if err == nil || !strings.Contains(err.Error(), "add pricing.openrouter/vendor/model") {
		t.Errorf("Parse error = %v, want pricing fix", err)
	}
}

func TestParse_PricingValidationAndRedactedOutput(t *testing.T) {
	t.Parallel()
	cfg, err := config.ParseWithEnv([]byte(strings.TrimSpace(`server:
  api_keys: [sk-test-local-ci]
providers:
  openrouter: {api_key: "${OPENROUTER_API_KEY}"}
pricing:
  openrouter/anthropic/claude: {input_usd_per_mtok: 3.000001, output_usd_per_mtok: 15}
`)), func(name string) (string, bool) {
		if name == "OPENROUTER_API_KEY" {
			return "sk-test-openrouter", true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	price := cfg.Pricing["openrouter/anthropic/claude"]
	if price.InputMicroUSDPerMillion != 3_000_001 || price.OutputMicroUSDPerMillion != 15_000_000 {
		t.Fatalf("price = %+v, want integer micro-USD per million", price)
	}
	var output strings.Builder
	if err := cfg.WriteRedacted(&output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "input_usd_per_mtok: 3.000001") || !strings.Contains(output.String(), "output_usd_per_mtok: 15") {
		t.Errorf("check output omitted exact declared prices:\n%s", output.String())
	}
}

func TestParse_PricingRejectsInvalidEntries(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ pricing, want string }{
		{"openrouter/model: {input_usd_per_mtok: 1}", "pricing.openrouter/model.output_usd_per_mtok is required"},
		{"unknown/model: {input_usd_per_mtok: 1, output_usd_per_mtok: 2}", "provider unknown is not configured"},
		{"openrouter/model: {input_usd_per_mtok: -1, output_usd_per_mtok: 2}", "decimal from 0 to 10000"},
		{"openrouter/model: {input_usd_per_mtok: 10000.000001, output_usd_per_mtok: 2}", "decimal from 0 to 10000"},
		{"openrouter/model: {input_usd_per_mtok: 1.0000001, output_usd_per_mtok: 2}", "decimal from 0 to 10000"},
	} {
		t.Run(tt.pricing, func(t *testing.T) {
			t.Parallel()
			data := "server:\n  api_keys: [sk-test-local-ci]\nproviders:\n  openrouter: {api_key: \"${OPENROUTER_API_KEY}\"}\npricing:\n  " + tt.pricing + "\n"
			_, err := config.ParseWithEnv([]byte(data), func(name string) (string, bool) {
				if name == "OPENROUTER_API_KEY" {
					return "sk-test-openrouter", true
				}
				return "", false
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestParse_StoreConfigExpandsEnvironment(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "usage.db")
	lookup := func(name string) (string, bool) {
		values := map[string]string{"STORE_PATH": path, "STORE_RETENTION": "30d"}
		value, ok := values[name]
		return value, ok
	}
	cfg, err := config.ParseWithEnv([]byte("server:\n  api_keys: [sk-test-local-ci]\nproviders:\n  mock: {}\nstore:\n  path: \"${STORE_PATH}\"\n  retention: \"${STORE_RETENTION}\"\n"), lookup)
	if err != nil {
		t.Fatalf("ParseWithEnv: %v", err)
	}
	if cfg.Store.Path != path || cfg.Store.Retention != 30*24*time.Hour {
		t.Errorf("Store = %+v, want expanded path and 30 day retention", cfg.Store)
	}
}

func TestParse_StoreValidation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tests := []struct {
		name, store, want string
	}{
		{name: "empty path", store: "path: \"\"", want: "store.path"},
		{name: "missing directory", store: "path: " + filepath.Join(dir, "missing", "usage.db"), want: "directory must exist"},
		{name: "directory path", store: "path: " + dir, want: "must name a file"},
		{name: "retention too short", store: "path: " + filepath.Join(dir, "usage.db") + "\n  retention: 23h", want: "store.retention"},
		{name: "retention too long", store: "path: " + filepath.Join(dir, "usage.db") + "\n  retention: 3651d", want: "store.retention"},
		{name: "unknown field", store: "path: " + filepath.Join(dir, "usage.db") + "\n  enabled: true", want: "store (line"},
		{name: "duration syntax", store: "path: " + filepath.Join(dir, "usage.db") + "\n  retention: someday", want: "store.retention"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Parse([]byte("server:\n  api_keys: [sk-test-local-ci]\nproviders:\n  mock: {}\nstore:\n  " + tt.store + "\n"))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestParse_LocalKeyForms(t *testing.T) {
	t.Parallel()
	type want struct{ name, value string }
	tests := []struct {
		name    string
		entries string
		want    []want
	}{
		{name: "one plain", entries: `["a"]`, want: []want{{"key-1", "a"}}},
		{name: "two plain", entries: `["a", "b"]`, want: []want{{"key-1", "a"}, {"key-2", "b"}}},
		{name: "named", entries: `[{name: ci, key: a}]`, want: []want{{"ci", "a"}}},
		{
			name:    "mixed counts every position",
			entries: `["a", {name: ci, key: b}, "c"]`,
			want:    []want{{"key-1", "a"}, {"ci", "b"}, {"key-3", "c"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Parse([]byte("server:\n  api_keys: " + tt.entries + "\nproviders:\n  mock: {}\n"))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(cfg.Server.APIKeys) != len(tt.want) {
				t.Fatalf("got %d keys, want %d", len(cfg.Server.APIKeys), len(tt.want))
			}
			for i, w := range tt.want {
				got := cfg.Server.APIKeys[i]
				if got.Name != w.name || got.Key.Reveal() != w.value {
					t.Errorf("key %d = {%s, %s}, want {%s, %s}", i, got.Name, got.Key.Reveal(), w.name, w.value)
				}
			}
		})
	}
}

func TestParse_LocalKeyErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		entries string
		want    string
	}{
		{name: "missing", entries: "", want: "server.api_keys must list at least one local key"},
		{name: "empty list", entries: "  api_keys: []\n", want: "server.api_keys must list at least one local key"},
		{name: "scalar", entries: "  api_keys: sk-test-local-ci\n", want: "server.api_keys (line 2): must be a list"},
		{name: "nested list", entries: "  api_keys: [[sk-test-local-ci]]\n", want: "server.api_keys[0] (line 2)"},
		{name: "numeric local key", entries: "  api_keys: [123]\n", want: "server.api_keys[0] (line 2): must be a string"},
		{name: "null entry", entries: "  api_keys: [null]\n", want: "server.api_keys[0] (line 2)"},
		{name: "mapping without name", entries: "  api_keys: [{key: sk-test-local-ci}]\n", want: "server.api_keys[0].name is required"},
		{name: "mapping without key", entries: "  api_keys: [{name: ci}]\n", want: "server.api_keys[0].key is required"},
		{name: "unknown field", entries: "  api_keys: [{name: ci, key: sk-test-local-ci, scope: all}]\n", want: `server.api_keys[0] (line 2): unknown field`},
		// Security property: a key mistyped as a mapping key is never printed.
		{name: "key as mapping key with null value", entries: "  api_keys:\n    - sk-test-local-ci:\n", want: "server.api_keys[0] (line 3): unknown field"},
		{name: "key as mapping key with a value", entries: "  api_keys: [{sk-test-local-ci: x}]\n", want: "server.api_keys[0] (line 2): unknown field"},
		{name: "key as extra field", entries: "  api_keys: [{name: ci, key: sk-test-local-cursor, sk-test-local-ci: 1}]\n", want: "server.api_keys[0] (line 2): unknown field"},
		{name: "non-string name", entries: "  api_keys: [{name: [ci], key: sk-test-local-ci}]\n", want: "server.api_keys[0].name (line 2): must be a string"},
		{name: "empty key", entries: "  api_keys: [\"\"]\n", want: "server.api_keys[0].key must not be empty"},
		{name: "key with space", entries: "  api_keys: [\"sk-test-local ci\"]\n", want: "server.api_keys[0].key must not contain whitespace"},
		{name: "uppercase name", entries: "  api_keys: [{name: CI, key: sk-test-local-ci}]\n", want: `server.api_keys[0].name "CI" must be`},
		{name: "name with space", entries: "  api_keys: [{name: my app, key: sk-test-local-ci}]\n", want: `server.api_keys[0].name "my app" must be`},
		{name: "name with underscore", entries: "  api_keys: [{name: app_1, key: sk-test-local-ci}]\n", want: `server.api_keys[0].name "app_1" must be`},
		{name: "empty name", entries: "  api_keys: [{name: \"\", key: sk-test-local-ci}]\n", want: `server.api_keys[0].name "" must be`},
		{name: "65 character name", entries: "  api_keys: [{name: " + strings.Repeat("a", 65) + ", key: sk-test-local-ci}]\n", want: "server.api_keys[0].name"},
		{
			name:    "duplicate names",
			entries: "  api_keys: [{name: ci, key: sk-test-local-ci}, {name: ci, key: sk-test-local-cursor}]\n",
			want:    `server.api_keys[1].name "ci" duplicates server.api_keys[0]`,
		},
		{
			name:    "explicit name collides with generated",
			entries: "  api_keys: [sk-test-local-ci, sk-test-local-cursor, {name: key-2, key: sk-test-local-x}]\n",
			want:    `server.api_keys[2].name "key-2" duplicates server.api_keys[1]`,
		},
		{
			name:    "same key under two names",
			entries: "  api_keys: [{name: ci, key: sk-test-local-ci}, {name: cursor, key: sk-test-local-ci}]\n",
			want:    "server.api_keys[1].key duplicates server.api_keys[0].key",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Parse([]byte("server:\n" + tt.entries + "providers:\n  mock: {}\n"))
			if err == nil {
				t.Fatal("Parse succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want substring %q", err, tt.want)
			}
			for _, key := range []string{localKeyCI, localKeyCursor, "sk-test-local ci"} {
				if strings.Contains(err.Error(), key) {
					t.Errorf("error = %q, contains key %q", err, key)
				}
			}
		})
	}
}

// Security property: no listen address, loopback included, accepts a config
// without a local key (ADR 0003).
func TestParse_LocalKeyRequiredOnEveryListen(t *testing.T) {
	t.Parallel()
	listens := []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080", "0.0.0.0:8080", ":8080", "[::]:8080", "192.168.1.5:8080"}
	for _, listen := range listens {
		t.Run(listen, func(t *testing.T) {
			t.Parallel()
			_, err := config.Parse([]byte(fmt.Sprintf("server:\n  listen: %q\nproviders:\n  mock: {}\n", listen)))
			if err == nil {
				t.Fatal("Parse without a local key succeeded, want error")
			}
			for _, want := range []string{"server.api_keys", "ADR 0003"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want substring %q", err, want)
				}
			}

			_, err = config.Parse([]byte(fmt.Sprintf("server:\n  listen: %q\n  api_keys: [sk-test-local-ci]\nproviders:\n  mock: {}\n", listen)))
			if err != nil {
				t.Errorf("Parse with a local key: %v", err)
			}
		})
	}
}

func TestServer_Loopback(t *testing.T) {
	t.Parallel()
	tests := []struct {
		listen string
		want   bool
	}{
		{"127.0.0.1:8080", true},
		{"127.0.0.2:8080", true},
		{"localhost:8080", true},
		{"localhost.:8080", true},
		{"LOCALHOST:8080", true},
		{"[::1]:8080", true},
		{":8080", false},
		{"0.0.0.0:8080", false},
		{"[::]:8080", false},
		{"192.168.1.5:8080", false},
		{"example.com:8080", false},
	}
	for _, tt := range tests {
		t.Run(tt.listen, func(t *testing.T) {
			t.Parallel()
			if got := (config.Server{Listen: tt.listen}).Loopback(); got != tt.want {
				t.Errorf("Loopback() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestServer_Host(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"127.0.0.1:8080": "127.0.0.1",
		"[::1]:8080":     "::1",
		"localhost:8080": "localhost",
		":8080":          "",
		"nope":           "",
	}
	for listen, want := range tests {
		t.Run(listen, func(t *testing.T) {
			t.Parallel()
			if got := (config.Server{Listen: listen}).Host(); got != want {
				t.Errorf("Host() = %q, want %q", got, want)
			}
		})
	}
}

func TestParse_Errors(t *testing.T) {
	t.Parallel()
	const keys = "  api_keys: [sk-test-local-ci]\n"
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{name: "empty file", yaml: "", want: "config is empty"},
		{name: "unknown top-level field", yaml: "server:\n" + keys + "providers:\n  mock: {}\nextra: 1\n", want: "config (line 5): unknown field"},
		{name: "unknown server field", yaml: "server:\n" + keys + "  port: 1\nproviders:\n  mock: {}\n", want: "server (line 3): unknown field"},
		{name: "no providers", yaml: "server:\n" + keys, want: "providers must configure at least one provider"},
		{name: "unknown provider", yaml: "server:\n" + keys + "providers:\n  nope: {}\n", want: "providers: unknown provider"},
		{name: "provider key on mock", yaml: "server:\n" + keys + "providers:\n  mock: {api_key: x}\n", want: "providers.mock.api_key is not supported"},
		{name: "literal provider key", yaml: "server:\n" + keys + "providers:\n  groq: {api_key: sk-test-groq-literal}\n", want: "providers.groq.api_key (line 4): must use an environment reference"},
		{name: "provider config must be a mapping", yaml: "server:\n" + keys + "providers:\n  mock: arbitrary\n", want: "providers.mock (line 4): must be a mapping"},
		{name: "reserved spillway provider", yaml: "server:\n" + keys + "providers:\n  spillway: {}\n", want: "providers.spillway is reserved"},
		{name: "reserved auto provider", yaml: "server:\n" + keys + "providers:\n  auto: {}\n", want: "providers.auto is reserved"},
		{name: "missing provider key", yaml: "server:\n" + keys + "providers:\n  groq: {}\n", want: "providers.groq.api_key is required"},
		{name: "base URL userinfo", yaml: "server:\n" + keys + "providers:\n  mock: {base_url: \"https://user:pass@example.com\"}\n", want: "providers.mock.base_url"},
		{name: "base URL bad port", yaml: "server:\n" + keys + "providers:\n  mock: {base_url: \"https://example.com:abc\"}\n", want: "providers.mock.base_url"},
		{name: "insecure non-loopback base URL", yaml: "server:\n" + keys + "providers:\n  mock: {base_url: \"http://example.com\"}\n", want: "providers.mock.base_url must use https"},
		{name: "invalid timeout", yaml: "server:\n" + keys + "providers:\n  mock: {timeout: 0s}\n", want: "providers.mock.timeout"},
		{name: "invalid log level", yaml: "server:\n" + keys + "logging: {level: verbose}\nproviders:\n  mock: {}\n", want: "logging.level"},
		{name: "router mode requires auto", yaml: "server:\n" + keys + "providers:\n  mock: {}\nrouter: {mode: rules}\n", want: "router.auto is required"},
		{name: "cache semantic milestone", yaml: "server:\n" + keys + "providers:\n  mock: {}\ncache: {semantic: true}\n", want: `cache.semantic is not supported`},
		{name: "bad listen", yaml: "server:\n  listen: nope\n" + keys + "providers:\n  mock: {}\n", want: "server.listen must be host:port"},
		{name: "port out of range", yaml: "server:\n  listen: \"127.0.0.1:70000\"\n" + keys + "providers:\n  mock: {}\n", want: "server.listen"},
		{name: "port not a number", yaml: "server:\n  listen: \"127.0.0.1:http\"\n" + keys + "providers:\n  mock: {}\n", want: "server.listen"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Parse([]byte(tt.yaml))
			if err == nil {
				t.Fatal("Parse succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want substring %q", err, tt.want)
			}
		})
	}
}

func TestParse_TiersValidateZeroSpendEligibility(t *testing.T) {
	t.Parallel()
	base := "server:\n  api_keys: [sk-test-local-ci]\nproviders:\n"
	tests := []struct {
		name      string
		providers string
		tier      string
		wantError string
	}{
		{name: "ollama is free", providers: "  ollama: {}\n", tier: "  cheap:\n    models: [ollama/llama3.2:3b]\n"},
		{name: "mock is free", providers: "  mock: {}\n", tier: "  cheap:\n    models: [mock/echo]\n"},
		{name: "openrouter free suffix", providers: "  openrouter: {api_key: \"${OPENROUTER_API_KEY}\"}\n", tier: "  cheap:\n    models: [openrouter/vendor/model:free]\n"},
		{name: "attested groq", providers: "  groq: {api_key: \"${GROQ_API_KEY}\", free_tier: true}\n", tier: "  cheap:\n    models: [groq/model]\n"},
		{name: "attested gemini", providers: "  gemini: {api_key: \"${GEMINI_API_KEY}\", free_tier: true}\n", tier: "  cheap:\n    models: [gemini/model]\n"},
		{name: "spend is allowed", providers: "  openrouter: {api_key: \"${OPENROUTER_API_KEY}\"}\n", tier: "  paid:\n    allow_spend: true\n    models: [openrouter/vendor/model]\n"},
		{name: "paid openrouter is rejected", providers: "  openrouter: {api_key: \"${OPENROUTER_API_KEY}\"}\n", tier: "  cheap:\n    models: [openrouter/vendor/model]\n", wantError: "tiers.cheap.models[0]: openrouter/vendor/model is not zero-spend eligible"},
		{name: "paid openrouter can opt in", providers: "  openrouter: {api_key: \"${OPENROUTER_API_KEY}\"}\n", tier: "  cheap:\n    allow_spend: true\n    models: [openrouter/vendor/model]\n"},
		{name: "unattested groq is rejected", providers: "  groq: {api_key: \"${GROQ_API_KEY}\"}\n", tier: "  cheap:\n    models: [groq/model]\n", wantError: "providers.groq.free_tier: true only if"},
		{name: "unattested groq can opt in", providers: "  groq: {api_key: \"${GROQ_API_KEY}\"}\n", tier: "  cheap:\n    allow_spend: true\n    models: [groq/model]\n"},
		{name: "unattested gemini is rejected", providers: "  gemini: {api_key: \"${GEMINI_API_KEY}\"}\n", tier: "  cheap:\n    models: [gemini/model]\n", wantError: "providers.gemini.free_tier: true only if"},
		{name: "unattested gemini can opt in", providers: "  gemini: {api_key: \"${GEMINI_API_KEY}\"}\n", tier: "  cheap:\n    allow_spend: true\n    models: [gemini/model]\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.ParseWithEnv([]byte(base+tt.providers+"tiers:\n"+tt.tier), func(name string) (string, bool) {
				return "sk-test-" + strings.ToLower(name), true
			})
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("ParseWithEnv: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Errorf("ParseWithEnv error = %v, want %q", err, tt.wantError)
			}
		})
	}
}

func TestParse_VirtualRoutesRejectDistinctInstancesOfOneService(t *testing.T) {
	t.Parallel()
	base := `server: {api_keys: [sk-test-local-ci]}
providers:
  gemini: {api_key: "${GEMINI_API_KEY}"}
  gemini-research: {type: gemini, api_key: "${GEMINI_RESEARCH_KEY}"}
  groq: {api_key: "${GROQ_API_KEY}"}
`
	lookup := func(name string) (string, bool) { return "sk-test-" + strings.ToLower(name), true }
	tests := []struct {
		name      string
		extra     string
		wantError bool
	}{
		{name: "same instance multiple models allowed", extra: "tiers:\n  mixed:\n    allow_spend: true\n    models: [gemini-research/a, gemini-research/b]\nrouter: {auto: [mixed]}\n"},
		{name: "different services allowed", extra: "tiers:\n  mixed:\n    allow_spend: true\n    models: [gemini/a, groq/b]\nrouter: {auto: [mixed]}\n"},
		{name: "two instances same service rejected in tier", extra: "tiers:\n  mixed:\n    allow_spend: true\n    models: [gemini/a, gemini-research/b]\nrouter: {auto: [mixed]}\n", wantError: true},
		{name: "two instances across auto tiers rejected", extra: "tiers:\n  first: {allow_spend: true, models: [gemini/a]}\n  second: {allow_spend: true, models: [gemini-research/b]}\nrouter: {auto: [first, second]}\n", wantError: true},
		{name: "two instances across rule difficulty rejected", extra: "tiers:\n  first: {allow_spend: true, models: [gemini/a]}\n  second: {allow_spend: true, models: [gemini-research/b]}\nrouter:\n  mode: rules\n  auto: [first, second]\n  difficulty: {simple: [first, second], moderate: [first], hard: [second]}\n  rules: [{when: {tools_present: true}, difficulty: simple}]\n", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.ParseWithEnv([]byte(base+tt.extra), lookup)
			if !tt.wantError {
				if err != nil {
					t.Fatalf("ParseWithEnv: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("ParseWithEnv accepted a virtual route containing distinct instances of one service")
			}
		})
	}
}

func TestParse_ProviderInstancesRetainAndSeparateCredentialReferences(t *testing.T) {
	t.Parallel()
	base := []byte(`server: {api_keys: [sk-test-local-ci]}
providers:
  gemini: {api_key: "${GEMINI_API_KEY}"}
  gemini-research: {type: gemini, api_key: "${GEMINI_RESEARCH_API_KEY}"}
`)
	cfg, err := config.ParseWithEnv(base, func(name string) (string, bool) { return "sk-test-" + strings.ToLower(name), true })
	if err != nil {
		t.Fatalf("ParseWithEnv: %v", err)
	}
	if got := cfg.Providers["gemini-research"].APIKeyEnv; got != "GEMINI_RESEARCH_API_KEY" {
		t.Fatalf("APIKeyEnv = %q, want separate variable name", got)
	}
	shared := strings.ReplaceAll(string(base), "GEMINI_RESEARCH_API_KEY", "GEMINI_API_KEY")
	_, err = config.ParseWithEnv([]byte(shared), func(name string) (string, bool) { return "sk-test-" + strings.ToLower(name), true })
	if err == nil {
		t.Fatal("ParseWithEnv accepted multiple instances that reference the same environment variable")
	}
}

func TestIsZeroSpendEligible_ADR0005Rules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		ref      providers.ModelRef
		provider config.Provider
		want     bool
	}{
		{name: "ollama", ref: providers.ModelRef{Provider: "ollama", Model: "llama3"}, want: true},
		{name: "mock", ref: providers.ModelRef{Provider: "mock", Model: "echo"}, want: true},
		{name: "openrouter free", ref: providers.ModelRef{Provider: "openrouter", Model: "vendor/model:free"}, want: true},
		{name: "openrouter paid", ref: providers.ModelRef{Provider: "openrouter", Model: "vendor/model"}},
		{name: "groq attested", ref: providers.ModelRef{Provider: "groq", Model: "model"}, provider: config.Provider{FreeTier: true}, want: true},
		{name: "groq not attested", ref: providers.ModelRef{Provider: "groq", Model: "model"}},
		{name: "gemini attested", ref: providers.ModelRef{Provider: "gemini", Model: "model"}, provider: config.Provider{FreeTier: true}, want: true},
		{name: "gemini not attested", ref: providers.ModelRef{Provider: "gemini", Model: "model"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := config.IsZeroSpendEligible(tt.ref, tt.provider); got != tt.want {
				t.Errorf("IsZeroSpendEligible(%+v, %+v) = %t, want %t", tt.ref, tt.provider, got, tt.want)
			}
		})
	}
}

func TestParse_FreeTierOnlyAppliesToGroqAndGemini(t *testing.T) {
	t.Parallel()
	for _, provider := range []string{"openrouter", "ollama", "mock"} {
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			input := "server:\n  api_keys: [sk-test-local-ci]\nproviders:\n  " + provider + ": {free_tier: true}\n"
			if provider == "openrouter" {
				input = strings.Replace(input, "{free_tier: true}", "{api_key: \"${OPENROUTER_API_KEY}\", free_tier: true}", 1)
			}
			_, err := config.ParseWithEnv([]byte(input), func(string) (string, bool) { return "sk-test-openrouter", true })
			if err == nil || !strings.Contains(err.Error(), "free_tier is only supported for groq and gemini") {
				t.Errorf("ParseWithEnv error = %v, want unsupported free_tier diagnostic", err)
			}
		})
	}
}

func TestParse_TierShapeValidation(t *testing.T) {
	t.Parallel()
	const base = "server:\n  api_keys: [sk-test-local-ci]\nproviders:\n  mock: {}\n"
	tests := []struct{ tier, want string }{
		{"  auto:\n    models: [mock/echo]\n", "tiers.auto is reserved"},
		{"  Upper:\n    models: [mock/echo]\n", "tiers.Upper.name must match"},
		{"  empty:\n    models: []\n", "tiers.empty.models must contain 1 to 16"},
		{"  many:\n    models: [" + strings.TrimSuffix(strings.Repeat("mock/echo, ", 17), ", ") + "]\n", "tiers.many.models must contain 1 to 16"},
		{"  bad:\n    models: [broken]\n", "tiers.bad.models[0]: model reference must be provider/model"},
		{"  missing:\n    models: [groq/model]\n", "tiers.missing.models[0]: provider groq is not configured"},
		{"  duplicate:\n    models: [mock/echo, mock/echo]\n", "tiers.duplicate.models[1] duplicates"},
		{"  free: [mock/echo]\n", "tiers.free.models (line"},
		{"  unknown:\n    models: [mock/echo]\n    fallback: true\n", "tiers.unknown (line"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			_, err := config.Parse([]byte(base + "tiers:\n" + tt.tier))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse error = %v, want %q", err, tt.want)
			}
		})
	}
}

const routingBase = "server:\n  api_keys: [sk-test-local-ci]\nproviders:\n  mock: {}\n  openrouter: {api_key: \"${OPENROUTER_API_KEY}\"}\n" +
	"tiers:\n  free:\n    models: [mock/echo, mock/rate-limited]\n  smart:\n    allow_spend: true\n    models: [openrouter/paid, mock/echo]\n"

func parseRouting(extra string) (config.Config, error) {
	return config.ParseWithEnv([]byte(routingBase+extra), func(string) (string, bool) { return "sk-test-openrouter-key", true })
}

func TestParse_ModelsAndRouter(t *testing.T) {
	t.Parallel()
	cfg, err := parseRouting("models:\n  mock/echo: {capabilities: [tools, json_mode], context_window: 32768}\n  mock/rate-limited: {capabilities: []}\nrouter:\n  auto: [free, smart]\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	echo := cfg.Models["mock/echo"]
	if strings.Join(echo.Capabilities, ",") != "tools,json_mode" || echo.ContextWindow != 32768 {
		t.Errorf("Models[mock/echo] = %+v, want tools,json_mode and 32768", echo)
	}
	if limited, ok := cfg.Models["mock/rate-limited"]; !ok || len(limited.Capabilities) != 0 || limited.ContextWindow != 0 {
		t.Errorf("Models[mock/rate-limited] = %+v, %v; want a text-only declaration", limited, ok)
	}
	if strings.Join(cfg.Router.Auto, ",") != "free,smart" {
		t.Errorf("Router.Auto = %v, want [free smart]", cfg.Router.Auto)
	}
	var out strings.Builder
	if err := cfg.WriteRedacted(&out); err != nil {
		t.Fatalf("WriteRedacted: %v", err)
	}
	for _, want := range []string{"\nmodels:", "context_window: 32768", "\nrouter:", "auto:"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("WriteRedacted output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestParse_WithoutModelsAndRouter(t *testing.T) {
	t.Parallel()
	cfg, err := parseRouting("")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Models) != 0 || len(cfg.Router.Auto) != 0 {
		t.Errorf("Models = %v, Router = %+v; want both empty", cfg.Models, cfg.Router)
	}
	var out strings.Builder
	if err := cfg.WriteRedacted(&out); err != nil {
		t.Fatalf("WriteRedacted: %v", err)
	}
	if strings.Contains(out.String(), "\nmodels:") || strings.Contains(out.String(), "\nrouter:") {
		t.Errorf("WriteRedacted prints empty routing sections:\n%s", out.String())
	}
}

func TestParse_ModelsAndRouterValidation(t *testing.T) {
	t.Parallel()
	refs := make([]string, 17)
	for i := range refs {
		refs[i] = fmt.Sprintf("mock/m%d", i)
	}
	const head = "server:\n  api_keys: [sk-test-local-ci]\nproviders:\n  mock: {}\n"
	big := head + "tiers:\n  a:\n    models: [" + strings.Join(refs[:9], ", ") + "]\n  b:\n    models: [" + strings.Join(refs[9:], ", ") + "]\nrouter:\n  auto: [a, b]\n"
	tests := []struct{ name, extra, full, want string }{
		{name: "models must be a mapping", extra: "models: [mock/echo]\n", want: "models (line"},
		{name: "declaration must be a mapping", extra: "models:\n  mock/echo: [tools]\n", want: "models.mock/echo (line"},
		{name: "unknown declaration field", extra: "models:\n  mock/echo: {capabilities: [], vision: true}\n", want: "models.mock/echo (line"},
		{name: "capabilities required", extra: "models:\n  mock/echo: {context_window: 8}\n", want: "models.mock/echo.capabilities is required; list what the model supports, or [] for text only"},
		{name: "capabilities must be a list", extra: "models:\n  mock/echo: {capabilities: tools}\n", want: "models.mock/echo.capabilities (line"},
		{name: "unknown capability", extra: "models:\n  mock/echo: {capabilities: [audio]}\n", want: `models.mock/echo.capabilities[0]: unknown capability "audio"; use tools, vision, json_mode or structured_outputs`},
		{name: "duplicate capability", extra: "models:\n  mock/echo: {capabilities: [tools, tools]}\n", want: "models.mock/echo.capabilities[1] duplicates"},
		{name: "context window zero", extra: "models:\n  mock/echo: {capabilities: [], context_window: 0}\n", want: "models.mock/echo.context_window (line"},
		{name: "context window too large", extra: "models:\n  mock/echo: {capabilities: [], context_window: 10000001}\n", want: "must be a whole number from 1 to 10000000"},
		{name: "context window not a number", extra: "models:\n  mock/echo: {capabilities: [], context_window: big}\n", want: "must be a whole number from 1 to 10000000"},
		{name: "bad model reference", extra: "models:\n  broken: {capabilities: []}\n", want: "models.broken: model reference must be provider/model"},
		{name: "model not in a tier", extra: "models:\n  mock/unavailable: {capabilities: []}\n", want: "models.mock/unavailable is not in any tier; capability declarations only affect tier and auto routing, so add it to a tier or remove it"},
		{name: "router must be a mapping", extra: "router: [free]\n", want: "router (line"},
		{name: "router auto required", extra: "router: {}\n", want: "router.auto is required"},
		{name: "router unknown field", extra: "router: {auto: [free], fallback: x}\n", want: "router (line"},
		{name: "router judge milestone", extra: "router: {auto: [free], judge_model: ollama/x}\n", want: "router.judge_model is not supported until v0.5"},
		{name: "router threshold milestone", extra: "router: {auto: [free], confidence_threshold: 0.7}\n", want: "router.confidence_threshold is not supported until v0.5"},
		{name: "router auto must be a list", extra: "router: {auto: free}\n", want: "router.auto (line"},
		{name: "router auto empty", extra: "router: {auto: []}\n", want: "router.auto must list at least one tier"},
		{name: "router auto unknown tier", extra: "router: {auto: [free, nope]}\n", want: "router.auto[1]: tier nope is not configured"},
		{name: "router auto duplicate", extra: "router: {auto: [free, free]}\n", want: "router.auto[1] duplicates"},
		{name: "router auto spend first", extra: "router: {auto: [smart, free]}\n", want: "router.auto[0]: tier smart allows spend and must come after every zero-spend tier (ADR 0008)"},
		{name: "router auto too many candidates", full: big, want: "router.auto resolves to 17 candidates; at most 16 are allowed"},
		{name: "rules require opt-in", extra: "router:\n  auto: [free]\n  rules: [{when: {tools_present: true}, difficulty: hard}]\n", want: "require router.mode: rules"},
		{name: "rules require every difficulty", extra: "router:\n  auto: [free]\n  mode: rules\n  difficulty: {moderate: [free]}\n  rules: [{when: {tools_present: true}, difficulty: hard}]\n", want: "router.difficulty.simple must list at least one tier"},
		{name: "unknown difficulty label", extra: "router:\n  auto: [free]\n  mode: rules\n  difficulty: {simple: [free], moderate: [free], hard: [free], trivial: [free]}\n  rules: [{when: {tools_present: true}, difficulty: hard}]\n", want: "router.difficulty (line"},
		{name: "difficulty tier must be in auto", extra: "router:\n  auto: [free]\n  mode: rules\n  difficulty: {simple: [smart], moderate: [free], hard: [free]}\n  rules: [{when: {tools_present: true}, difficulty: hard}]\n", want: "router.difficulty.simple[0]: tier smart is not listed in router.auto"},
		{name: "empty rule condition", extra: "router:\n  auto: [free]\n  mode: rules\n  difficulty: {simple: [free], moderate: [free], hard: [free]}\n  rules: [{when: {}, difficulty: hard}]\n", want: "router.rules[0].when must specify at least one condition"},
		{name: "unbounded threshold", extra: "router:\n  auto: [free]\n  mode: rules\n  difficulty: {simple: [free], moderate: [free], hard: [free]}\n  rules: [{when: {estimated_input_tokens_gte: 2000001}, difficulty: hard}]\n", want: "must be from 1 to 2000000"},
		{name: "unknown rule condition", extra: "router:\n  auto: [free]\n  mode: rules\n  difficulty: {simple: [free], moderate: [free], hard: [free]}\n  rules: [{when: {prompt_contains: tool}, difficulty: hard}]\n", want: "router.rules[0].when (line"},
		{name: "duplicate rule conditions", extra: "router:\n  auto: [free]\n  mode: rules\n  difficulty: {simple: [free], moderate: [free], hard: [free]}\n  rules:\n    - when: {tools_present: true}\n      difficulty: simple\n    - when: {tools_present: true}\n      difficulty: hard\n", want: "router.rules[1] duplicates conditions from rule 0"},
		{name: "invalid difficulty label", extra: "router:\n  auto: [free]\n  mode: rules\n  difficulty: {simple: [free], moderate: [free], hard: [free]}\n  rules: [{when: {tools_present: true}, difficulty: trivial}]\n", want: "router.rules[0].difficulty must be simple, moderate or hard"},
		{name: "tier reference with space", full: head + "tiers:\n  a:\n    models: [\"mock/echo two\"]\n", want: "tiers.a.models[0]: model reference must be printable ASCII without spaces"},
		{name: "tier reference with control character", full: head + "tiers:\n  a:\n    models: [\"mock/echo\\x01\"]\n", want: "tiers.a.models[0]: model reference must be printable ASCII without spaces"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var err error
			if tt.full != "" {
				_, err = config.Parse([]byte(tt.full))
			} else {
				_, err = parseRouting(tt.extra)
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse error = %v, want %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "sk-test-") {
				t.Errorf("Parse error %q contains a key", err)
			}
		})
	}
}

func TestParse_RulesRouter(t *testing.T) {
	t.Parallel()
	cfg, err := parseRouting("router:\n  mode: rules\n  auto: [free, smart]\n  difficulty:\n    simple: [free]\n    moderate: [free, smart]\n    hard: [smart]\n  rules:\n    - when: {tools_present: true}\n      difficulty: hard\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Router.Mode != "rules" || len(cfg.Router.Rules) != 1 || cfg.Router.Rules[0].Difficulty != "hard" || len(cfg.Router.Difficulty["moderate"]) != 2 {
		t.Fatalf("Router = %+v, want rules mode with one rule and configured fallback tiers", cfg.Router)
	}
}

func TestParse_LocalClassifierRequiresConfiguredPinnedOllama(t *testing.T) {
	t.Parallel()
	const base = `server:
  api_keys: [sk-test-local-ci]
providers:
  mock: {}
  ollama: {base_url: "http://localhost:11434"}
tiers:
  free:
    models: [mock/echo]
router:
  mode: rules
  auto: [free]
  difficulty: {simple: [free], moderate: [free], hard: [free]}
  classifier: {model: "ollama/llama3.2:3b"}
`
	cfg, err := config.Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Router.Classifier == nil || cfg.Router.Classifier.Model != "ollama/llama3.2:3b" {
		t.Fatalf("Classifier = %+v, want configured pinned Ollama model", cfg.Router.Classifier)
	}
	for _, tt := range []struct{ name, replace, with, want string }{
		{name: "missing provider", replace: "  ollama: {base_url: \"http://localhost:11434\"}\n", want: "explicitly configured providers.ollama"},
		{name: "unpinned model", replace: "ollama/llama3.2:3b", with: "ollama/llama3.2", want: "pinned ollama model reference"},
		{name: "latest model", replace: "ollama/llama3.2:3b", with: "ollama/llama3.2:latest", want: "pinned ollama model reference"},
		{name: "wrong provider", replace: "ollama/llama3.2:3b", with: "mock/echo:3b", want: "pinned ollama model reference"},
		{name: "remote host", replace: "http://localhost:11434", with: "https://ollama.example", want: "loopback host"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			input := base
			if tt.name == "missing provider" {
				input = strings.Replace(input, tt.replace, "", 1)
			} else {
				input = strings.Replace(input, tt.replace, tt.with, 1)
			}
			if _, err := config.Parse([]byte(input)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestParse_RulesRouterRejectsDuplicateFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		extra string
	}{
		{
			name:  "duplicate rule condition",
			extra: "router:\n  mode: rules\n  auto: [free]\n  difficulty: {simple: [free], moderate: [free], hard: [free]}\n  rules:\n    - when:\n        tools_present: true\n        tools_present: false\n      difficulty: hard\n",
		},
		{
			name:  "duplicate rule difficulty",
			extra: "router:\n  mode: rules\n  auto: [free]\n  difficulty: {simple: [free], moderate: [free], hard: [free]}\n  rules:\n    - when: {tools_present: true}\n      difficulty: simple\n      difficulty: hard\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseRouting(tt.extra); err == nil {
				t.Fatal("Parse succeeded with duplicate fields, want error")
			}
		})
	}
}

func TestParse_RouterAutoCountsSharedModelsOnce(t *testing.T) {
	t.Parallel()
	refs := make([]string, 16)
	for i := range refs {
		refs[i] = fmt.Sprintf("mock/m%d", i)
	}
	input := "server:\n  api_keys: [sk-test-local-ci]\nproviders:\n  mock: {}\ntiers:\n  a:\n    models: [" + strings.Join(refs, ", ") + "]\n  b:\n    models: [mock/m0, mock/m15]\nrouter:\n  auto: [a, b]\n"
	if _, err := config.Parse([]byte(input)); err != nil {
		t.Fatalf("Parse: %v, want shared models counted once", err)
	}
}

func TestExamples_ConfigsValidate(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"config.yaml", "config.mock.yaml"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			_, err := config.LoadWithEnv(filepath.Join("..", "..", "examples", path), func(name string) (string, bool) {
				return "sk-test-" + strings.ToLower(name), true
			})
			if err != nil {
				t.Fatalf("LoadWithEnv: %v", err)
			}
		})
	}
}

func TestParse_OllamaRejectsProviderKey(t *testing.T) {
	t.Parallel()
	_, err := config.ParseWithEnv([]byte("server:\n  api_keys: [sk-test-local-ci]\nproviders:\n  ollama:\n    api_key: \"${OLLAMA_API_KEY}\"\n"), func(name string) (string, bool) {
		return "sk-test-ollama-key", name == "OLLAMA_API_KEY"
	})
	if err == nil || !strings.Contains(err.Error(), "providers.ollama.api_key is not supported") {
		t.Fatalf("ParseWithEnv error = %v, want Ollama api_key rejection", err)
	}
}

func TestConfig_FmtNeverRevealsSecrets(t *testing.T) {
	t.Parallel()
	cfg, err := config.Parse([]byte("server:\n  api_keys: [sk-test-local-ci, {name: cursor, key: sk-test-local-cursor}]\nproviders:\n  mock: {}\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	values := map[string]any{"config": cfg, "server": cfg.Server, "local key": cfg.Server.APIKeys[1]}
	for name, v := range values {
		for _, verb := range []string{"%v", "%+v", "%#v"} {
			got := fmt.Sprintf(verb, v)
			for _, key := range []string{localKeyCI, localKeyCursor} {
				if strings.Contains(got, key) {
					t.Errorf("%s %s = %q, contains key", name, verb, got)
				}
			}
		}
	}
}

func TestLoad_ReadsFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "server:\n  api_keys: [sk-test-local-ci]\nproviders:\n  mock: {}\n")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.APIKeys[0].Name != "key-1" {
		t.Errorf("APIKeys[0].Name = %q, want key-1", cfg.Server.APIKeys[0].Name)
	}
}

func TestLoad_FullExampleWithEnvironment(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", "examples", "config.yaml")
	cfg, err := config.LoadWithEnv(path, func(name string) (string, bool) {
		return "sk-test-" + strings.ToLower(name), true
	})
	if err != nil {
		t.Fatalf("LoadWithEnv: %v", err)
	}
	if cfg.Logging.Level != "info" || cfg.Providers["groq"].Timeout.String() != "2m0s" {
		t.Errorf("defaults: logging=%q groq timeout=%s", cfg.Logging.Level, cfg.Providers["groq"].Timeout)
	}
	if len(cfg.Providers) != 4 || cfg.Server.APIKeys[0].Key.Reveal() != "sk-test-spillway_key" {
		t.Errorf("full example did not expand expected providers and local key")
	}
	for _, name := range []string{"gemini", "groq", "ollama", "openrouter"} {
		if _, ok := cfg.Providers[name]; !ok {
			t.Errorf("legacy provider %q was not preserved", name)
		}
	}
	for name, provider := range cfg.Providers {
		if provider.Type != "" {
			t.Errorf("legacy provider %q type = %q, want implicit preset resolution", name, provider.Type)
		}
	}
	if len(cfg.Tiers) != 1 || strings.Join(cfg.Tiers["free"].Models, ",") != "ollama/llama3.2:3b" {
		t.Errorf("legacy free tier = %+v, want ollama/llama3.2:3b", cfg.Tiers)
	}
	if len(cfg.Models) != 0 || len(cfg.Router.Auto) != 0 {
		t.Errorf("legacy model declarations or auto route changed: models=%v auto=%v", cfg.Models, cfg.Router.Auto)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "missing.yaml")

	_, err := config.Load(path)

	if err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("error = %v, want it to name %s", err, path)
	}
}

func TestLoad_FileTooLarge(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "big.yaml")
	writeFile(t, path, "# "+strings.Repeat("x", 1<<20)+"\n")

	_, err := config.Load(path)

	if err == nil || !strings.Contains(err.Error(), "larger than 1 MiB") {
		t.Errorf("error = %v, want a size error", err)
	}
}

func TestParse_ExpandsEnvironment(t *testing.T) {
	t.Parallel()
	input := []byte("server:\n  api_keys: [\"${SPILLWAY_KEY}\"]\nproviders:\n  groq:\n    api_key: \"${GROQ_API_KEY}\"\n")
	lookup := func(name string) (string, bool) {
		values := map[string]string{"SPILLWAY_KEY": "sk-test-local", "GROQ_API_KEY": "sk-test-groq"}
		value, ok := values[name]
		return value, ok
	}
	cfg, err := config.ParseWithEnv(input, lookup)
	if err != nil {
		t.Fatalf("ParseWithEnv: %v", err)
	}
	if got := cfg.Server.APIKeys[0].Key.Reveal(); got != "sk-test-local" {
		t.Errorf("local key = %q, want expanded key", got)
	}
	if got := cfg.Providers["groq"].APIKey.Reveal(); got != "sk-test-groq" {
		t.Errorf("provider key = %q, want expanded key", got)
	}
}

func TestParse_UnsetEnvironmentNamesPathAndVariable(t *testing.T) {
	t.Parallel()
	_, err := config.ParseWithEnv([]byte("server:\n  api_keys: [\"${SPILLWAY_KEY}\"]\nproviders:\n  mock: {}\n"), func(string) (string, bool) { return "", false })
	if err == nil {
		t.Fatal("ParseWithEnv succeeded, want unset variable error")
	}
	for _, want := range []string{"server.api_keys[0]", "line 2", "SPILLWAY_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want %q", err, want)
		}
	}
}

func TestParseWithEnv_ReportsEveryMissingVariable(t *testing.T) {
	t.Parallel()
	input := []byte("server:\n  api_keys: [\"${SPILLWAY_KEY}\"]\nlogging:\n  level: \"${SET_VALUE}\"\nproviders:\n  groq:\n    api_key: \"${GROQ_API_KEY}\"\n  gemini:\n    api_key: \"${GEMINI_API_KEY}\"\n")
	values := map[string]string{"SET_VALUE": "sk-test-set-secret"}
	_, err := config.ParseWithEnv(input, func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	})
	if err == nil {
		t.Fatal("ParseWithEnv succeeded, want missing environment variable errors")
	}
	for _, want := range []string{
		"server.api_keys[0] (line 2)", "SPILLWAY_KEY",
		"providers.groq.api_key (line 7)", "GROQ_API_KEY",
		"providers.gemini.api_key (line 9)", "GEMINI_API_KEY",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want %q", err, want)
		}
	}
	for _, secret := range values {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error = %q, must not contain environment values", err)
		}
	}
}

func TestParse_HistoricalExampleConfigsStillLoad(t *testing.T) {
	t.Parallel()
	root := filepath.Join("testdata", "compat")
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".yaml" {
			return nil
		}
		data, err := os.ReadFile(path) // #nosec G304,G122 -- fixtures are repository-owned testdata.
		if err != nil {
			return err
		}
		_, err = config.ParseWithEnv(data, func(name string) (string, bool) {
			return "sk-test-" + strings.ToLower(name), true
		})
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestParse_HistoricalConfigsWithEnvironmentStoreStillLoad(t *testing.T) {
	t.Parallel()
	paths, err := filepath.Glob(filepath.Join("testdata", "compat-store", "*", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no compat-store fixtures found")
	}
	for _, path := range paths {
		data, err := os.ReadFile(path) // #nosec G304 -- fixtures are repository-owned testdata.
		if err != nil {
			t.Fatal(err)
		}
		// Their store directory exists only where they run (the image, an evaluation run), so only store.path moves.
		data = withStorePath(t, path, data, filepath.ToSlash(filepath.Join(t.TempDir(), "spillway.db")))
		_, err = config.ParseWithEnv(data, func(name string) (string, bool) {
			return "sk-test-" + strings.ToLower(name), true
		})
		if err != nil {
			t.Errorf("parse %s: %v", path, err)
		}
	}
}

func withStorePath(t *testing.T, name string, data []byte, storePath string) []byte {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "store" {
			continue
		}
		store := root.Content[i+1]
		for j := 0; j+1 < len(store.Content); j += 2 {
			if store.Content[j].Value == "path" {
				store.Content[j+1].Value = storePath
				out, err := yaml.Marshal(&doc)
				if err != nil {
					t.Fatalf("encode %s: %v", name, err)
				}
				return out
			}
		}
	}
	t.Fatalf("%s has no store.path", name)
	return nil
}

func TestParse_EnvironmentExpansionIsScalar(t *testing.T) {
	t.Parallel()
	input := []byte("server:\n  api_keys: [\"${KEY}\"]\nproviders:\n  mock: {}\n")
	lookup := func(name string) (string, bool) {
		return "sk-test-expanded\nproviders:\n  groq: {api_key: injected}", true
	}
	_, err := config.ParseWithEnv(input, lookup)
	if err == nil {
		t.Fatal("ParseWithEnv succeeded, want local key whitespace error")
	}
	if !strings.Contains(err.Error(), "server.api_keys[0].key must not contain whitespace") {
		t.Errorf("error = %q, want scalar local key validation", err)
	}
	if strings.Contains(err.Error(), "providers.groq") {
		t.Errorf("error = %q, environment value changed YAML structure", err)
	}
}

func TestParse_EnvironmentReferenceSyntax(t *testing.T) {
	t.Parallel()
	input := []byte("server:\n  api_keys: [\"$${LITERAL}\", \"$PLAIN\"]\nproviders:\n  mock: {}\n")
	cfg, err := config.ParseWithEnv(input, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("ParseWithEnv: %v", err)
	}
	if got := cfg.Server.APIKeys[0].Key.Reveal(); got != "${LITERAL}" {
		t.Errorf("escaped reference = %q, want literal ${LITERAL}", got)
	}
	if got := cfg.Server.APIKeys[1].Key.Reveal(); got != "$PLAIN" {
		t.Errorf("bare dollar reference = %q, want literal $PLAIN", got)
	}
}

func TestParse_EnvironmentReferenceErrorsNamePath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		ref    string
		lookup func(string) (string, bool)
		want   string
	}{
		{name: "empty value", ref: "${EMPTY}", lookup: func(string) (string, bool) { return "", true }, want: "environment variable EMPTY is not set"},
		{name: "invalid name", ref: "${BAD-NAME}", lookup: func(string) (string, bool) { return "x", true }, want: "invalid environment variable name"},
		{name: "unterminated", ref: "${BROKEN", lookup: func(string) (string, bool) { return "x", true }, want: "unterminated environment variable reference"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			input := []byte("server:\n  api_keys: [\"" + tt.ref + "\"]\nproviders:\n  mock: {}\n")
			_, err := config.ParseWithEnv(input, tt.lookup)
			if err == nil {
				t.Fatal("ParseWithEnv succeeded, want environment reference error")
			}
			for _, want := range []string{"server.api_keys[0]", "line 2", tt.want} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want %q", err, want)
				}
			}
		})
	}
}

func TestParse_BudgetShapeErrorPrecedesExpansion(t *testing.T) {
	t.Parallel()
	_, err := config.ParseWithEnv([]byte("budgets: \"${UNSET}\"\n"), func(string) (string, bool) { return "", false })
	if err == nil || !strings.Contains(err.Error(), "budgets") || !strings.Contains(err.Error(), "must be a mapping") {
		t.Errorf("ParseWithEnv error = %v, want budgets shape error", err)
	}
}

func TestConfig_WriteRedacted(t *testing.T) {
	t.Parallel()
	cfg, err := config.ParseWithEnv([]byte("server:\n  api_keys: [\"${LOCAL_KEY}\"]\nproviders:\n  groq:\n    api_key: \"${PROVIDER_KEY}\"\n"), func(name string) (string, bool) {
		return map[string]string{"LOCAL_KEY": localKeyCI, "PROVIDER_KEY": "sk-test-groq-secret"}[name], true
	})
	if err != nil {
		t.Fatalf("ParseWithEnv: %v", err)
	}
	var out strings.Builder
	if err := cfg.WriteRedacted(&out); err != nil {
		t.Fatalf("WriteRedacted: %v", err)
	}
	var roundTrip map[string]any
	if err := yaml.Unmarshal([]byte(out.String()), &roundTrip); err != nil {
		t.Errorf("redacted config is not valid YAML: %v", err)
	}
	for _, want := range []string{"[REDACTED]", "key-1", "timeout: 2m0s"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("redacted config = %q, want %q", out.String(), want)
		}
	}
	for _, secret := range []string{localKeyCI, "sk-test-groq-secret"} {
		if strings.Contains(out.String(), secret) {
			t.Errorf("redacted config contains a secret: %q", out.String())
		}
	}
}

func TestParse_ValidationErrorScrubsConfiguredSecrets(t *testing.T) {
	t.Parallel()
	secret := "sk-test-secret-listen"
	input := []byte("server:\n  listen: \"${KEY}\"\n  api_keys: [\"${KEY}\"]\nproviders:\n  mock: {}\n")
	_, err := config.ParseWithEnv(input, func(string) (string, bool) { return secret, true })
	if err == nil {
		t.Fatal("ParseWithEnv succeeded, want invalid listen error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error contains configured secret: %q", err)
	}
	if !strings.Contains(err.Error(), "server.listen") {
		t.Errorf("error = %q, want server.listen path", err)
	}
}

func TestParse_UnknownFieldErrorDoesNotEchoMappingKey(t *testing.T) {
	t.Parallel()
	secret := "sk-test-secret-field"
	input := []byte("server:\n  api_keys: [sk-test-local-safe]\nproviders:\n  mock:\n    " + secret + ": \"${MISSING}\"\n")
	_, err := config.ParseWithEnv(input, func(string) (string, bool) { return "", false })
	if err == nil {
		t.Fatal("ParseWithEnv succeeded, want unknown provider field error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("decode error contains configured secret: %q", err)
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("error = %q, want unknown field diagnostic", err)
	}
}

func TestParse_LiteralProviderKeyNeverAppearsInError(t *testing.T) {
	t.Parallel()
	providerKey := "example-literal-provider-value"
	input := []byte("server:\n  api_keys: [sk-test-local-safe]\nproviders:\n  groq: {api_key: " + providerKey + "}\n")
	_, err := config.ParseWithEnv(input, func(string) (string, bool) { return "", false })
	if err == nil {
		t.Fatal("ParseWithEnv succeeded, want provider key env-reference error")
	}
	if strings.Contains(err.Error(), providerKey) {
		t.Errorf("error contains literal provider key: %q", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadListenWithEnv_ReadsListenWithoutProviderEnv(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for name, test := range map[string]struct {
		server string
		want   string
	}{
		"explicit": {server: "  listen: 127.0.0.1:18089\n", want: "127.0.0.1:18089"},
		"expanded": {server: "  listen: ${SPILLWAY_LISTEN}\n", want: "127.0.0.1:19090"},
		"default":  {server: "", want: "127.0.0.1:8080"},
	} {
		path := filepath.Join(dir, name+".yaml")
		writeFile(t, path, "server:\n"+test.server+"  api_keys: [sk-test-local]\nproviders:\n  groq:\n    api_key: ${GROQ_API_KEY}\n")
		lookup := func(name string) (string, bool) {
			if name == "SPILLWAY_LISTEN" {
				return "127.0.0.1:19090", true
			}
			return "", false
		}

		got, err := config.LoadListenWithEnv(path, lookup)

		if err != nil || got != test.want {
			t.Errorf("%s: LoadListenWithEnv = %q, %v; want %q", name, got, err, test.want)
		}
	}
}
