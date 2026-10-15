// Package config loads and validates the Spillway YAML config.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Polqt/spillway/internal/redact"
	"go.yaml.in/yaml/v3"
)

// maxFileBytes caps the config file so a wrong path cannot exhaust memory.
const maxFileBytes = 1 << 20

const defaultListen = "127.0.0.1:8080"

// Config is a loaded, validated config.
type Config struct {
	Server    Server              `yaml:"server"`
	Logging   Logging             `yaml:"logging"`
	Providers map[string]Provider `yaml:"providers"`
	Tiers     map[string]Tier     `yaml:"tiers,omitempty"`
	// Models declares capabilities per model reference; it is the only
	// capability source (ADR 0007).
	Models  map[string]ModelDeclaration `yaml:"models,omitempty"`
	Router  Router                      `yaml:"router,omitempty"`
	Store   *Store                      `yaml:"store,omitempty"`
	Pricing map[string]Price            `yaml:"pricing,omitempty"`
}

// Price contains declared USD per million tokens in integer micro-USD.
type Price struct {
	InputMicroUSDPerMillion  int64 `yaml:"-"`
	OutputMicroUSDPerMillion int64 `yaml:"-"`
}

// UnmarshalYAML decodes explicit decimal USD-per-million prices without floats.
func (p *Price) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("pricing entry (line %d): must be a mapping", n.Line)
	}
	seen := make(map[string]bool, 2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, value := n.Content[i], n.Content[i+1]
		if key.Value != "input_usd_per_mtok" && key.Value != "output_usd_per_mtok" {
			return fmt.Errorf("pricing entry (line %d): unknown field %q", key.Line, key.Value)
		}
		if seen[key.Value] {
			return fmt.Errorf("pricing.%s (line %d): duplicate field", key.Value, key.Line)
		}
		seen[key.Value] = true
		micro, err := parsePrice(value)
		if err != nil {
			return fmt.Errorf("pricing.%s (line %d): must be a decimal from 0 to 10000 with at most 6 fractional digits", key.Value, value.Line)
		}
		if key.Value == "input_usd_per_mtok" {
			p.InputMicroUSDPerMillion = micro
		} else {
			p.OutputMicroUSDPerMillion = micro
		}
	}
	if !seen["input_usd_per_mtok"] || !seen["output_usd_per_mtok"] {
		return errors.New("pricing entry must include input_usd_per_mtok and output_usd_per_mtok")
	}
	return nil
}

// MarshalYAML writes prices as exact decimal YAML scalars.
func (p Price) MarshalYAML() (any, error) {
	return map[string]*yaml.Node{
		"input_usd_per_mtok":  priceNode(p.InputMicroUSDPerMillion),
		"output_usd_per_mtok": priceNode(p.OutputMicroUSDPerMillion),
	}, nil
}

func priceNode(micro int64) *yaml.Node {
	whole, fraction := micro/1_000_000, micro%1_000_000
	value := strconv.FormatInt(whole, 10)
	if fraction != 0 {
		value += "." + strings.TrimRight(fmt.Sprintf("%06d", fraction), "0")
	}
	tag := "!!int"
	if fraction != 0 {
		tag = "!!float"
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
}

func parsePrice(n *yaml.Node) (int64, error) {
	if n.Kind != yaml.ScalarNode || (n.Tag != "!!int" && n.Tag != "!!float" && n.Tag != "!!str") {
		return 0, errors.New("not a decimal")
	}
	value := n.Value
	if value == "" || strings.ContainsAny(value, "eE_+-") {
		return 0, errors.New("invalid decimal")
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 || parts[0] == "" || (len(parts) == 2 && (len(parts[1]) == 0 || len(parts[1]) > 6)) {
		return 0, errors.New("invalid decimal")
	}
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || whole > 10000 {
		return 0, errors.New("out of range")
	}
	fraction := int64(0)
	if len(parts) == 2 {
		fractionText := parts[1] + strings.Repeat("0", 6-len(parts[1]))
		fraction, err = strconv.ParseInt(fractionText, 10, 64)
		if err != nil {
			return 0, errors.New("invalid fraction")
		}
	}
	valueMicro := whole*1_000_000 + fraction
	if valueMicro > 10_000_000_000 {
		return 0, errors.New("out of range")
	}
	return valueMicro, nil
}

// Store configures the opt-in local usage database.
type Store struct {
	Path      string        `yaml:"path"`
	Retention time.Duration `yaml:"retention"`
}

// UnmarshalYAML accepts day-based retention values such as 90d.
func (s *Store) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("store (line %d): must be a mapping", n.Line)
	}
	s.Retention = 90 * 24 * time.Hour
	seen := make(map[string]bool, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		field, value := n.Content[i], n.Content[i+1]
		if field.Value != "path" && field.Value != "retention" {
			return fmt.Errorf("store (line %d): unknown field %q", field.Line, field.Value)
		}
		if seen[field.Value] {
			return fmt.Errorf("store.%s (line %d): duplicate field", field.Value, field.Line)
		}
		seen[field.Value] = true
		if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
			return fmt.Errorf("store.%s (line %d): must be a string", field.Value, value.Line)
		}
		if field.Value == "path" {
			s.Path = value.Value
			continue
		}
		retention, err := parseStoreRetention(value.Value)
		if err != nil {
			return fmt.Errorf("store.retention (line %d): must be a duration from 1d to 3650d", value.Line)
		}
		s.Retention = retention
	}
	return nil
}

// MarshalYAML writes retention using whole days when possible.
func (s Store) MarshalYAML() (any, error) {
	retention := s.Retention.String()
	if s.Retention%(24*time.Hour) == 0 {
		retention = fmt.Sprintf("%dd", s.Retention/(24*time.Hour))
	}
	return map[string]string{"path": s.Path, "retention": retention}, nil
}

func parseStoreRetention(value string) (time.Duration, error) {
	if strings.HasSuffix(value, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(value, "d"))
		if err != nil || days < 1 || days > 3650 {
			return 0, errors.New("invalid day count")
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(value)
}

// Tier is an ordered list of model references with an optional spend opt-in.
type Tier struct {
	Models     []string `yaml:"models"`
	AllowSpend bool     `yaml:"allow_spend"`
}

// ModelDeclaration is the complete list of what one model supports; an
// empty Capabilities list means text only.
type ModelDeclaration struct {
	Capabilities []string `yaml:"capabilities"`
	// ContextWindow is the model's context size in tokens, 0 when undeclared.
	ContextWindow int `yaml:"context_window,omitempty"`
}

// Router configures the auto virtual model.
type Router struct {
	// Auto lists tier names in the order auto tries them (ADR 0008).
	Auto []string `yaml:"auto,omitempty"`
}

// Logging configures the program's structured logger.
type Logging struct {
	Level string `yaml:"level"`
}

// Server configures the HTTP listener and its local keys.
type Server struct {
	Listen  string    `yaml:"listen"`
	APIKeys LocalKeys `yaml:"api_keys"`
}

// Host returns the host part of the listen address, without brackets for
// IPv6, or "" when the address is malformed or listens on every interface.
func (s Server) Host() string {
	host, _, err := net.SplitHostPort(s.Listen)
	if err != nil {
		return ""
	}
	return host
}

// Loopback reports whether the listen host is "localhost" or a loopback IP.
// An empty host listens on every interface and is not loopback.
func (s Server) Loopback() bool {
	host := s.Host()
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Provider configures one upstream provider.
type Provider struct {
	APIKey     redact.Secret `yaml:"api_key,omitempty"`
	BaseURL    string        `yaml:"base_url,omitempty"`
	Timeout    time.Duration `yaml:"timeout,omitempty"`
	FreeTier   bool          `yaml:"free_tier,omitempty"`
	timeoutSet bool
}

// UnmarshalYAML accepts durations as strings and keeps errors free of values.
func (p *Provider) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("provider config (line %d): must be a mapping", n.Line)
	}
	seen := make(map[string]struct{}, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		field, value := n.Content[i], n.Content[i+1]
		if field.Value != "api_key" && field.Value != "base_url" && field.Value != "timeout" && field.Value != "free_tier" {
			return fmt.Errorf("provider config (line %d): unknown field", field.Line)
		}
		if field.Value == "free_tier" {
			if value.Kind != yaml.ScalarNode || value.Tag != "!!bool" {
				return fmt.Errorf("providers.free_tier (line %d): must be true or false", value.Line)
			}
			if _, ok := seen[field.Value]; ok {
				return fmt.Errorf("providers.free_tier (line %d): duplicate field", field.Line)
			}
			seen[field.Value] = struct{}{}
			p.FreeTier = value.Value == "true"
			continue
		}
		if value.Kind != yaml.ScalarNode || value.Tag == "!!null" || value.Tag != "!!str" {
			return fmt.Errorf("providers.%s (line %d): must be a string", field.Value, value.Line)
		}
		if _, ok := seen[field.Value]; ok {
			return fmt.Errorf("providers.%s (line %d): duplicate field", field.Value, field.Line)
		}
		seen[field.Value] = struct{}{}
		switch field.Value {
		case "api_key":
			p.APIKey = redact.NewSecret(value.Value)
		case "base_url":
			p.BaseURL = value.Value
		case "timeout":
			d, err := time.ParseDuration(value.Value)
			if err != nil {
				return fmt.Errorf("provider.timeout (line %d): must be a duration such as 30s", value.Line)
			}
			p.Timeout = d
			p.timeoutSet = true
		}
	}
	return nil
}

// MarshalYAML writes durations in their human-readable YAML form.
func (p Provider) MarshalYAML() (any, error) {
	value := map[string]any{"timeout": p.Timeout.String()}
	if p.FreeTier {
		value["free_tier"] = true
	}
	if !p.APIKey.IsZero() {
		value["api_key"] = p.APIKey
	}
	if p.BaseURL != "" {
		value["base_url"] = p.BaseURL
	}
	return value, nil
}

// Load reads, parses and validates the config file at path.
func Load(path string) (Config, error) {
	return LoadWithEnv(path, os.LookupEnv)
}

// LoadWithEnv reads, expands, parses and validates the config at path.
func LoadWithEnv(path string, lookupEnv func(string) (string, bool)) (Config, error) {
	f, err := os.Open(path) // #nosec G304 -- the path is the operator's own -config flag
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return Config{}, fmt.Errorf("config: read %s: %w", path, err)
	}
	if len(data) > maxFileBytes {
		return Config{}, fmt.Errorf("config: %s is larger than 1 MiB", path)
	}
	return ParseWithEnv(data, lookupEnv)
}

// Parse decodes and validates a config. Unknown fields are errors.
func Parse(data []byte) (Config, error) {
	return ParseWithEnv(data, os.LookupEnv)
}

// ParseWithEnv expands environment references, decodes and validates config.
func ParseWithEnv(data []byte, lookupEnv func(string) (string, bool)) (Config, error) {
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return Config{}, fmt.Errorf("config: invalid YAML: %w", err)
	}
	if len(node.Content) == 0 {
		return Config{}, errors.New("config: config is empty")
	}
	if lookupEnv == nil {
		lookupEnv = func(string) (string, bool) { return "", false }
	}
	if err := rejectFutureSections(node.Content[0]); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	if err := rejectUnknownFields(node.Content[0]); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	if err := expandConfig(node.Content[0], lookupEnv); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	var expanded bytes.Buffer
	enc := yaml.NewEncoder(&expanded)
	if err := enc.Encode(&node); err != nil {
		return Config{}, fmt.Errorf("config: encode expanded YAML: %w", err)
	}
	if err := enc.Close(); err != nil {
		return Config{}, fmt.Errorf("config: encode expanded YAML: %w", err)
	}
	dec := yaml.NewDecoder(&expanded)
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return Config{}, errors.New("config: config is empty")
		}
		return Config{}, fmt.Errorf("config: %w", redact.NewScrubber(secretsFromNode(node.Content[0])...).Error(err))
	}
	if cfg.Server.Listen == "" {
		cfg.Server.Listen = defaultListen
	}
	if cfg.Logging.Level == "" {
		cfg.Logging.Level = "info"
	}
	if cfg.Store != nil && cfg.Store.Retention == 0 {
		cfg.Store.Retention = 90 * 24 * time.Hour
	}
	for name, provider := range cfg.Providers {
		if !provider.timeoutSet {
			provider.Timeout = 120 * time.Second
			cfg.Providers[name] = provider
		}
	}
	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("config: %w", redact.NewScrubber(cfg.Secrets()...).Error(err))
	}
	return cfg, nil
}

// Secrets returns all provider and local keys configured in c.
func (c Config) Secrets() []redact.Secret {
	secrets := make([]redact.Secret, 0, len(c.Providers)+len(c.Server.APIKeys))
	for _, provider := range c.Providers {
		if !provider.APIKey.IsZero() {
			secrets = append(secrets, provider.APIKey)
		}
	}
	for _, key := range c.Server.APIKeys {
		if !key.Key.IsZero() {
			secrets = append(secrets, key.Key)
		}
	}
	return secrets
}

// WriteRedacted writes the validated configuration with all keys redacted.
func (c Config) WriteRedacted(w io.Writer) error {
	enc := yaml.NewEncoder(w)
	if err := enc.Encode(c); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return enc.Close()
}
