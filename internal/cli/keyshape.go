package cli

import (
	"fmt"
	"strings"
)

func certainKeyMistake(value string) string {
	switch {
	case value != strings.TrimSpace(value):
		return "has leading or trailing whitespace"
	case strings.HasPrefix(value, "\"") || strings.HasSuffix(value, "\"") || strings.HasPrefix(value, "'") || strings.HasSuffix(value, "'"):
		return "has surrounding quotes"
	case strings.HasPrefix(strings.ToLower(value), "bearer "):
		return "includes a Bearer prefix; provide only the provider key"
	case strings.Contains(value, "${"):
		return "contains a literal environment reference; set the variable to its key value"
	case looksLikePlaceholder(value):
		return "looks like a placeholder rather than a key"
	default:
		return ""
	}
}

func looksLikePlaceholder(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{"<your-provider-key>", "your-api-key", "your api key", "replace-me", "replace_with", "placeholder"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func keyFormatWarning(provider, value string) string {
	expected := map[string]string{"gemini": "AIza", "groq": "gsk_", "openrouter": "sk-or-"}[provider]
	if expected == "" || strings.HasPrefix(value, expected) {
		return ""
	}
	return fmt.Sprintf("does not start with the usual public prefix %q; provider formats may change", expected)
}
