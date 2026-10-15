package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/router"
	"github.com/Polqt/spillway/internal/store"
)

func (rec *requestRecord) setRequestedModel(model string) {
	rec.usage.RequestedModel = model
}

func (rec *requestRecord) addPlanSkips(skips []router.Skip) {
	for _, skip := range skips {
		rec.addSkip(skip.Ref, fixedSkipReason(skip.Reason))
	}
}

func (rec *requestRecord) addSkip(ref providers.ModelRef, reason string) {
	if len(rec.usage.Skips) >= 16 {
		return
	}
	rec.usage.Skips = append(rec.usage.Skips, store.Skip{Provider: ref.Provider, Model: ref.Model, Reason: reason})
}

func fixedSkipReason(reason string) string {
	switch {
	case strings.HasPrefix(reason, "lacks "):
		return "capabilities_missing"
	case reason == "capabilities not declared":
		return "capabilities_undeclared"
	case strings.HasPrefix(reason, "context too small"):
		return "context_too_small"
	case reason == "cooling":
		return "cooldown"
	case reason == "budget exceeded":
		return "budget_exceeded"
	default:
		return "unavailable"
	}
}

func (rec *requestRecord) setError(err error) {
	rec.err = err
	if errors.Is(err, context.Canceled) {
		rec.usage.ErrorKind = "client_disconnected"
		return
	}
	rec.usage.ErrorKind = candidateErrorKind(err)
}

func recordResponseUsage(body []byte, usage *store.Request) {
	var response struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &response) != nil || response.Usage == nil {
		usage.UsageSource = "none"
		return
	}
	usage.PromptTokens = response.Usage.PromptTokens
	usage.CompletionTokens = response.Usage.CompletionTokens
	usage.UsageSource = "reported"
}

func setPlanErrorKind(err error) string {
	switch {
	case errors.Is(err, router.ErrUnknownTier), errors.Is(err, router.ErrAutoNotConfigured):
		return "model_not_found"
	case errors.Is(err, router.ErrInvalidField):
		return "invalid_request"
	case errors.Is(err, router.ErrNoCapableModel):
		return "no_capable_model"
	default:
		return "internal_error"
	}
}
