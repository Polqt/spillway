// Package evaluation runs paired, offline or live routing evaluations without
// sending evaluation content to Spillway's normal logs or usage store.
package evaluation

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	maxDatasetBytes = 8 << 20
	maxPrompts      = 1000
	maxPromptBytes  = 32 << 10
)

// PromptSet is a versioned evaluation corpus.
type PromptSet struct {
	Version int      `json:"version"`
	Prompts []Prompt `json:"prompts"`
}

// Prompt contains evaluation content and the labels used for grouped metrics.
type Prompt struct {
	ID         string                     `json:"id"`
	Category   string                     `json:"category"`
	Difficulty string                     `json:"difficulty"`
	Prompt     string                     `json:"prompt"`
	Settings   map[string]json.RawMessage `json:"settings,omitempty"`
}

// LoadPromptSet reads and validates one version 1 prompt set.
func LoadPromptSet(r io.Reader) (PromptSet, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxDatasetBytes+1))
	if err != nil {
		return PromptSet{}, fmt.Errorf("read prompt set: %w", err)
	}
	if len(data) > maxDatasetBytes {
		return PromptSet{}, errors.New("prompt set exceeds 8 MiB")
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var set PromptSet
	if err := dec.Decode(&set); err != nil {
		return PromptSet{}, fmt.Errorf("decode prompt set: %w", err)
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return PromptSet{}, errors.New("prompt set must contain one JSON value")
	}
	if set.Version != 1 {
		return PromptSet{}, fmt.Errorf("prompt set version %d is unsupported; want 1", set.Version)
	}
	if len(set.Prompts) == 0 || len(set.Prompts) > maxPrompts {
		return PromptSet{}, fmt.Errorf("prompt set prompts must contain 1 to %d entries", maxPrompts)
	}
	seen := make(map[string]struct{}, len(set.Prompts))
	for i, prompt := range set.Prompts {
		if prompt.ID == "" || len(prompt.ID) > 80 {
			return PromptSet{}, fmt.Errorf("prompt %d id must contain 1 to 80 bytes", i+1)
		}
		if _, duplicate := seen[prompt.ID]; duplicate {
			return PromptSet{}, fmt.Errorf("prompt %d has duplicate id %q", i+1, prompt.ID)
		}
		seen[prompt.ID] = struct{}{}
		if !label(prompt.Category) {
			return PromptSet{}, fmt.Errorf("prompt %q category must be a lowercase label", prompt.ID)
		}
		if prompt.Difficulty != "simple" && prompt.Difficulty != "moderate" && prompt.Difficulty != "hard" {
			return PromptSet{}, fmt.Errorf("prompt %q difficulty must be simple, moderate, or hard", prompt.ID)
		}
		if strings.TrimSpace(prompt.Prompt) == "" || len(prompt.Prompt) > maxPromptBytes {
			return PromptSet{}, fmt.Errorf("prompt %q text must contain 1 to %d bytes", prompt.ID, maxPromptBytes)
		}
		if err := validateSettings(prompt.ID, prompt.Settings); err != nil {
			return PromptSet{}, err
		}
	}
	return set, nil
}

func label(value string) bool {
	if value == "" || len(value) > 40 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func validateSettings(id string, settings map[string]json.RawMessage) error {
	for key, raw := range settings {
		switch key {
		case "temperature", "top_p":
			var value float64
			maxValue := 2.0
			if key == "top_p" {
				maxValue = 1
			}
			if err := json.Unmarshal(raw, &value); err != nil || value < 0 || value > maxValue {
				return fmt.Errorf("prompt %q setting %s must be a number from 0 to %g", id, key, maxValue)
			}
		case "max_tokens":
			var value int
			if err := json.Unmarshal(raw, &value); err != nil || value < 1 || value > 32768 {
				return fmt.Errorf("prompt %q setting max_tokens must be an integer from 1 to 32768", id)
			}
		case "seed":
			var value int64
			if err := json.Unmarshal(raw, &value); err != nil {
				return fmt.Errorf("prompt %q setting seed must be an integer", id)
			}
		case "stop":
			var value any
			if err := json.Unmarshal(raw, &value); err != nil {
				return fmt.Errorf("prompt %q setting stop must be a string or string list", id)
			}
			if _, ok := value.(string); !ok {
				items, ok := value.([]any)
				if !ok || len(items) > 4 {
					return fmt.Errorf("prompt %q setting stop must be a string or list of at most 4 strings", id)
				}
				for _, item := range items {
					if _, ok := item.(string); !ok {
						return fmt.Errorf("prompt %q setting stop must contain only strings", id)
					}
				}
			}
		default:
			return fmt.Errorf("prompt %q has unsupported setting %q", id, key)
		}
	}
	return nil
}
