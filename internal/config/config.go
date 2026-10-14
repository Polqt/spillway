// Package config loads and validates the Spillway YAML config.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"
)

// maxFileBytes caps the config file so a wrong path cannot exhaust memory.
const maxFileBytes = 1 << 20

const defaultListen = "127.0.0.1:8080"

// Config is a loaded, validated config.
type Config struct {
	Server    Server              `yaml:"server"`
	Providers map[string]Provider `yaml:"providers"`
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
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Provider configures one provider. The mock provider takes no settings.
type Provider struct{}

// Load reads, parses and validates the config file at path.
func Load(path string) (Config, error) {
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
	return Parse(data)
}

// Parse decodes and validates a config. Unknown fields are errors.
func Parse(data []byte) (Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return Config{}, errors.New("config: config is empty")
		}
		return Config{}, fmt.Errorf("config: %w", err)
	}
	if cfg.Server.Listen == "" {
		cfg.Server.Listen = defaultListen
	}
	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}
