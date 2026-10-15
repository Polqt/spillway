// Package budget contains integer cost estimation shared by usage recording and budgets.
package budget

import (
	"encoding/json"
	"math/big"
	"strings"

	"github.com/Polqt/spillway/internal/config"
	"github.com/Polqt/spillway/internal/providers"
)

// Cost is an attempt's estimated spend and the source used to derive it.
type Cost struct {
	MicroUSD int64
	Source   string
}

// Pricer estimates one attempt using ADR 0005 eligibility and configured prices.
type Pricer struct {
	pricing   map[string]config.Price
	providers map[string]config.Provider
}

// NewPricer creates a per-attempt cost estimator from validated configuration.
func NewPricer(cfg config.Config) *Pricer {
	return &Pricer{pricing: cfg.Pricing, providers: cfg.Providers}
}

// Estimate applies free, reported, declared, then unknown source precedence.
func (p *Pricer) Estimate(ref providers.ModelRef, promptTokens, completionTokens int64, reportedCost json.RawMessage) Cost {
	provider := p.providers[ref.Provider]
	if config.IsZeroSpendEligible(ref, provider) {
		return Cost{Source: "free"}
	}
	if cost, ok := reportedUSDToMicro(reportedCost); ok {
		return Cost{MicroUSD: cost, Source: "reported"}
	}
	price, ok := p.pricing[ref.Provider+"/"+ref.Model]
	if !ok {
		return Cost{Source: "unknown"}
	}
	total := new(big.Int).Mul(big.NewInt(max(promptTokens, 0)), big.NewInt(price.InputMicroUSDPerMillion))
	total.Add(total, new(big.Int).Mul(big.NewInt(max(completionTokens, 0)), big.NewInt(price.OutputMicroUSDPerMillion)))
	divisor := big.NewInt(1_000_000)
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(total, divisor, remainder)
	if remainder.Sign() != 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() {
		return Cost{MicroUSD: int64(^uint64(0) >> 1), Source: "declared"}
	}
	return Cost{MicroUSD: quotient.Int64(), Source: "declared"}
}

func reportedUSDToMicro(raw json.RawMessage) (int64, bool) {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return 0, false
	}
	amount, ok := new(big.Rat).SetString(value)
	if !ok || amount.Sign() < 0 {
		return 0, false
	}
	microUSD := new(big.Rat).Mul(amount, new(big.Rat).SetInt64(1_000_000))
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(microUSD.Num(), microUSD.Denom(), remainder)
	if remainder.Sign() != 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() {
		return 0, false
	}
	return quotient.Int64(), true
}
