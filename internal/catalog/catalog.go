// Package catalog discovers advisory model metadata from configured providers.
package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/Polqt/spillway/internal/config"
	"github.com/Polqt/spillway/internal/providers"
	"go.yaml.in/yaml/v3"
)

const maxResponseBytes = 8 << 20

// Options supplies HTTP dependencies for catalog requests.
type Options struct {
	HTTPClient *http.Client
}

type suggestion struct {
	capabilities []string
	context      int
	inputPrice   string
	outputPrice  string
	source       string
}

// Discover fetches only configured catalog providers and writes suggestions.
func Discover(ctx context.Context, cfg config.Config, out io.Writer, options Options) error {
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	} else {
		clientCopy := *client
		clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &clientCopy
	}
	refs := configuredRefs(cfg)
	found := make(map[string]suggestion)
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		provider, enabled := cfg.Providers[name]
		if !enabled {
			continue
		}
		serviceType := providerServiceType(name, provider)
		if serviceType != "openrouter" && serviceType != "ollama" && serviceType != "groq" {
			continue
		}
		base := provider.BaseURL
		if base == "" {
			base = defaultURL(serviceType)
		}
		if serviceType == "ollama" {
			base = strings.TrimSuffix(strings.TrimRight(base, "/"), "/v1")
			for _, ref := range refs {
				if ref.Provider != name {
					continue
				}
				body, _ := json.Marshal(map[string]string{"model": ref.Model})
				data, err := fetch(ctx, client, provider, base, "/api/show", http.MethodPost, body)
				if err != nil {
					return fmt.Errorf("fetch ollama catalog for %s: %w", ref.Model, err)
				}
				item, err := parseOllama(data)
				if err != nil {
					return fmt.Errorf("decode ollama catalog for %s: %w", ref.Model, err)
				}
				item.source = "Ollama /api/show"
				item.inputPrice, item.outputPrice = "0", "0"
				found[ref.String()] = item
			}
			continue
		}
		data, err := fetch(ctx, client, provider, base, "/models", http.MethodGet, nil)
		if err != nil {
			return fmt.Errorf("fetch %s catalog: %w", name, err)
		}
		items, err := parseOpenAIList(data, serviceType)
		if err != nil {
			return fmt.Errorf("decode %s catalog: %w", name, err)
		}
		for _, item := range items {
			key := name + "/" + item.id
			if _, wanted := refs[key]; wanted {
				found[key] = item.value
			}
		}
	}
	return writeOutput(out, cfg, refs, found)
}

func configuredRefs(cfg config.Config) map[string]providers.ModelRef {
	refs := make(map[string]providers.ModelRef)
	for _, tier := range cfg.Tiers {
		for _, model := range tier.Models {
			addRef(refs, model)
		}
	}
	for model := range cfg.Pricing {
		addRef(refs, model)
	}
	return refs
}

func addRef(refs map[string]providers.ModelRef, model string) {
	if ref, err := providers.ParseModelRef(model); err == nil {
		refs[model] = ref
	}
}

func defaultURL(name string) string {
	preset, _ := providers.LookupPreset(name)
	return preset.DefaultBaseURL
}

func fetch(parent context.Context, client *http.Client, provider config.Provider, base, path, method string, body []byte) ([]byte, error) {
	u, err := url.Parse(strings.TrimRight(base, "/") + path)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return nil, errors.New("invalid configured provider URL")
	}
	ctx, cancel := context.WithTimeout(parent, provider.Timeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, errors.New("build provider catalog request")
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if !provider.APIKey.IsZero() {
		req.Header.Set("Authorization", "Bearer "+provider.APIKey.Reveal())
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("provider catalog request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("provider returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, errors.New("read provider catalog response")
	}
	if len(data) > maxResponseBytes {
		return nil, errors.New("provider catalog response exceeds 8 MiB")
	}
	return data, nil
}

type openRouterModel struct {
	ID      string `json:"id"`
	Pricing struct {
		Prompt     string `json:"prompt"`
		Completion string `json:"completion"`
	} `json:"pricing"`
	Parameters   []string `json:"supported_parameters"`
	Architecture struct {
		InputModalities []string `json:"input_modalities"`
	} `json:"architecture"`
	Context int `json:"context_length"`
}

func parseOpenAIList(data []byte, provider string) ([]struct {
	id    string
	value suggestion
}, error) {
	var list struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	result := make([]struct {
		id    string
		value suggestion
	}, 0, len(list.Data))
	for _, raw := range list.Data {
		if provider == "groq" {
			var item struct {
				ID      string `json:"id"`
				Context int    `json:"context_window"`
			}
			if err := json.Unmarshal(raw, &item); err != nil {
				return nil, err
			}
			if item.ID != "" {
				result = append(result, struct {
					id    string
					value suggestion
				}{item.ID, suggestion{context: item.Context, source: "Groq /models"}})
			}
			continue
		}
		var item openRouterModel
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		if item.ID == "" {
			continue
		}
		caps := make([]string, 0, 4)
		for _, p := range item.Parameters {
			switch p {
			case "tools":
				caps = append(caps, "tools")
			case "json_schema":
				caps = append(caps, "structured_outputs")
			case "response_format":
				caps = append(caps, "json_mode")
			}
		}
		if slices.Contains(item.Architecture.InputModalities, "image") {
			caps = append(caps, "vision")
		}
		slices.Sort(caps)
		caps = slices.Compact(caps)
		in, err := perMillion(item.Pricing.Prompt)
		if err != nil {
			in = ""
		}
		out, err := perMillion(item.Pricing.Completion)
		if err != nil {
			out = ""
		}
		result = append(result, struct {
			id    string
			value suggestion
		}{item.ID, suggestion{capabilities: caps, context: item.Context, inputPrice: in, outputPrice: out, source: "OpenRouter /models"}})
	}
	return result, nil
}

func perMillion(value string) (string, error) {
	if value == "" || strings.ContainsAny(value, "+-eE/ ") {
		return "", errors.New("invalid catalog price")
	}
	rat, ok := new(big.Rat).SetString(value)
	if !ok || rat.Sign() < 0 {
		return "", errors.New("invalid catalog price")
	}
	rat.Mul(rat, big.NewRat(1_000_000, 1))
	// Round up to the config's six-decimal USD precision so discovery does not understate a rate.
	rat.Mul(rat, big.NewRat(1_000_000, 1))
	n := new(big.Int).Quo(rat.Num(), rat.Denom())
	if new(big.Int).Mod(rat.Num(), rat.Denom()).Sign() != 0 {
		n.Add(n, big.NewInt(1))
	}
	if n.Cmp(big.NewInt(10_000_000_000)) > 0 {
		return "", errors.New("catalog price exceeds config limit")
	}
	whole, frac := new(big.Int), new(big.Int)
	whole.QuoRem(n, big.NewInt(1_000_000), frac)
	if frac.Sign() == 0 {
		return whole.String(), nil
	}
	fraction := frac.String()
	fraction = strings.Repeat("0", 6-len(fraction)) + fraction
	return whole.String() + "." + strings.TrimRight(fraction, "0"), nil
}

func parseOllama(data []byte) (suggestion, error) {
	var item struct {
		Capabilities []string       `json:"capabilities"`
		ModelInfo    map[string]any `json:"model_info"`
	}
	if err := json.Unmarshal(data, &item); err != nil {
		return suggestion{}, err
	}
	var caps []string
	for _, c := range item.Capabilities {
		switch c {
		case "tools", "vision":
			caps = append(caps, c)
		case "json":
			caps = append(caps, "json_mode")
		}
	}
	slices.Sort(caps)
	caps = slices.Compact(caps)
	contextWindow := 0
	keys := make([]string, 0, len(item.ModelInfo))
	for key := range item.ModelInfo {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		value := item.ModelInfo[key]
		if strings.HasSuffix(key, ".context_length") {
			switch v := value.(type) {
			case float64:
				contextWindow = int(v)
			case json.Number:
				n, _ := strconv.Atoi(string(v))
				contextWindow = n
			}
			if contextWindow > 0 {
				break
			}
		}
	}
	return suggestion{capabilities: caps, context: contextWindow}, nil
}

func writeOutput(out io.Writer, cfg config.Config, refs map[string]providers.ModelRef, found map[string]suggestion) error {
	keys := make([]string, 0, len(refs))
	for key := range refs {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var b strings.Builder
	b.WriteString("# Suggested catalog declarations. Review before adding; catalog data does not affect routing.\n")
	b.WriteString("models:\n")
	for _, key := range keys {
		item, ok := found[key]
		if !ok {
			fmt.Fprintf(&b, "# no catalog result for %s\n", key)
			continue
		}
		if item.source == "" {
			continue
		}
		declared, declaredOK := cfg.Models[key]
		quoted, _ := yaml.Marshal(key)
		b.WriteString("  " + strings.TrimSpace(string(quoted)) + ":\n")
		fmt.Fprintf(&b, "    capabilities: [%s] # source: %s\n", strings.Join(item.capabilities, ", "), item.source)
		if item.context > 0 {
			fmt.Fprintf(&b, "    context_window: %d # source: %s\n", item.context, item.source)
		}
		if declaredOK {
			added := make([]string, 0)
			omitted := make([]string, 0)
			for _, capability := range item.capabilities {
				if !slices.Contains(declared.Capabilities, capability) {
					added = append(added, capability)
				}
			}
			for _, capability := range declared.Capabilities {
				if !slices.Contains(item.capabilities, capability) {
					omitted = append(omitted, capability)
				}
			}
			if len(added) > 0 || len(omitted) > 0 {
				fmt.Fprintf(&b, "    # declared %v", declared.Capabilities)
				if len(added) > 0 {
					fmt.Fprintf(&b, ", catalog adds %s", strings.Join(added, ", "))
				}
				if len(omitted) > 0 {
					fmt.Fprintf(&b, ", catalog omits %s", strings.Join(omitted, ", "))
				}
				b.WriteByte('\n')
			}
			if declared.ContextWindow != item.context && item.context > 0 {
				fmt.Fprintf(&b, "    # declared context_window: %d, catalog suggests %d\n", declared.ContextWindow, item.context)
			}
		}
	}
	b.WriteString("pricing:\n")
	for _, key := range keys {
		item, ok := found[key]
		if !ok {
			continue
		}
		serviceType := providerServiceType(refs[key].Provider, cfg.Providers[refs[key].Provider])
		if serviceType == "openrouter" && strings.HasSuffix(refs[key].Model, ":free") &&
			((item.inputPrice != "" && item.inputPrice != "0") || (item.outputPrice != "" && item.outputPrice != "0")) {
			fmt.Fprintf(&b, "# WARNING: %s has non-zero catalog price despite the :free suffix\n", key)
		}
		if item.inputPrice == "" || item.outputPrice == "" || (serviceType != "openrouter" && serviceType != "ollama") {
			continue
		}
		quoted, _ := yaml.Marshal(key)
		fmt.Fprintf(&b, "  %s:\n    input_usd_per_mtok: %s # source: %s\n    output_usd_per_mtok: %s # source: %s\n", strings.TrimSpace(string(quoted)), item.inputPrice, item.source, item.outputPrice, item.source)
		if declared, exists := cfg.Pricing[key]; exists {
			fmt.Fprintf(&b, "    # declared input_usd_per_mtok: %s, output_usd_per_mtok: %s\n", formatMicro(declared.InputMicroUSDPerMillion), formatMicro(declared.OutputMicroUSDPerMillion))
		}
	}
	for _, name := range providerNames(cfg.Providers) {
		serviceType := providerServiceType(name, cfg.Providers[name])
		if serviceType == "gemini" || serviceType == "mock" {
			label := serviceType
			if serviceType == "gemini" {
				label = "Gemini"
			}
			if name == serviceType {
				fmt.Fprintf(&b, "# %s skipped: no usable catalog fields\n", label)
			} else {
				fmt.Fprintf(&b, "# %s skipped for instance %s: no usable catalog fields\n", label, name)
			}
		}
	}
	_, err := io.WriteString(out, b.String())
	return err
}

func providerServiceType(name string, provider config.Provider) string {
	if provider.Type != "" {
		return provider.Type
	}
	return name
}

func providerNames(configured map[string]config.Provider) []string {
	names := make([]string, 0, len(configured))
	for name := range configured {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func formatMicro(micro int64) string {
	whole, fraction := micro/1_000_000, micro%1_000_000
	value := strconv.FormatInt(whole, 10)
	if fraction != 0 {
		value += "." + strings.TrimRight(fmt.Sprintf("%06d", fraction), "0")
	}
	return value
}
