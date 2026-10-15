package budget_test

import (
	"encoding/json"
	"testing"

	"github.com/Polqt/spillway/internal/budget"
	"github.com/Polqt/spillway/internal/config"
	"github.com/Polqt/spillway/internal/providers"
)

func TestPricer_EstimatesAttemptCost(t *testing.T) {
	pricer := budget.NewPricer(config.Config{
		Providers: map[string]config.Provider{"groq": {}},
		Pricing:   map[string]config.Price{"groq/model": {InputMicroUSDPerMillion: 3_000_000, OutputMicroUSDPerMillion: 15_000_000}},
	})
	tests := []struct {
		name     string
		ref      providers.ModelRef
		prompt   int64
		complete int64
		reported json.RawMessage
		want     budget.Cost
	}{
		{name: "worked example", ref: providers.ModelRef{Provider: "groq", Model: "model"}, prompt: 1234, complete: 567, want: budget.Cost{MicroUSD: 12207, Source: "declared"}},
		{name: "reported cost wins", ref: providers.ModelRef{Provider: "groq", Model: "model"}, prompt: 1234, complete: 567, reported: json.RawMessage("0.00001234"), want: budget.Cost{MicroUSD: 13, Source: "reported"}},
		{name: "reported cost exponent is valid JSON number", ref: providers.ModelRef{Provider: "groq", Model: "model"}, reported: json.RawMessage("1e-7"), want: budget.Cost{MicroUSD: 1, Source: "reported"}},
		{name: "free model wins over reported", ref: providers.ModelRef{Provider: "ollama", Model: "model"}, prompt: 1234, complete: 567, reported: json.RawMessage("12.50"), want: budget.Cost{Source: "free"}},
		{name: "unpriced paid model", ref: providers.ModelRef{Provider: "groq", Model: "other"}, want: budget.Cost{Source: "unknown"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pricer.Estimate(tt.ref, tt.prompt, tt.complete, tt.reported)
			if got != tt.want {
				t.Errorf("Estimate() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
