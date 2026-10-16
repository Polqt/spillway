package router

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Polqt/spillway/internal/providers"
)

var (
	// ErrUnknownTier means a requested spillway tier is not configured.
	ErrUnknownTier = errors.New("unknown tier")
	// ErrAutoNotConfigured means no auto tier chain is configured.
	ErrAutoNotConfigured = errors.New("auto is not configured")
	// ErrNoCapableModel means no candidate can serve the virtual request.
	ErrNoCapableModel = errors.New("no capable model")
)

const maxDifficultyRules = 32
const maxDifficultyTokenThreshold = 2_000_000

// Declaration describes the capabilities and context size the user declares
// for one model (ADR 0007).
type Declaration struct {
	Capabilities  []Capability
	ContextWindow int
}

// Options supplies the validated tier configuration used to build a Router.
type Options struct {
	Tiers        map[string][]providers.ModelRef
	Auto         []string
	Declarations map[providers.ModelRef]Declaration
	// DifficultyTiers maps each closed difficulty label to its ordered fallback tiers.
	DifficultyTiers map[string][]string
	// Rules are evaluated in order for auto requests; the first match wins.
	Rules []Rule
	// Classifier optionally selects a difficulty for auto requests before rules run.
	Classifier DifficultyClassifier
}

// Rule is a first-match difficulty rule over bounded request features.
type Rule struct {
	// EstimatedInputTokensGTE matches the estimated input size at or above this threshold.
	EstimatedInputTokensGTE *int
	// RequestedOutputTokensGTE matches the requested output size at or above this threshold.
	RequestedOutputTokensGTE *int
	// ToolsPresent matches whether tool definitions or tool history are present.
	ToolsPresent *bool
	// Difficulty is the closed label selected when all conditions match.
	Difficulty string
}

// Router resolves virtual models into ordered candidates.
type Router struct {
	tiers           map[string][]providers.ModelRef
	auto            []string
	declarations    map[providers.ModelRef]Declaration
	difficultyTiers map[string][]string
	rules           []Rule
	classifier      DifficultyClassifier
}

// Skip records why a configured candidate was excluded from a plan.
type Skip struct {
	Ref    providers.ModelRef
	Reason string
	size   bool
}

type candidate struct {
	tier string
	ref  providers.ModelRef
}

// Plan is the deterministic result of resolving one virtual model.
type Plan struct {
	Source string
	// Mode identifies the routing mode used for this plan.
	Mode string
	// Rule is the 1-based matching rule position, or 0 when the default matched.
	Rule int
	// Difficulty is the selected closed difficulty label.
	Difficulty string
	// ClassifierFallback marks deterministic routing after classification failed.
	ClassifierFallback bool
	// SelectedTier is the first configured tier for the selected difficulty.
	SelectedTier string
	Tiers        []string
	Needs        []Capability
	Eligible     []providers.ModelRef
	Skipped      []Skip
}

// NoCapableModelError reports why candidate selection left no eligible model.
type NoCapableModelError struct {
	message          string
	Skipped          []Skip
	EverySkipWasSize bool
}

// Error returns the human-readable reason no candidate was eligible.
func (e *NoCapableModelError) Error() string { return e.message }

// Unwrap identifies this as a no-capable-model planning failure.
func (e *NoCapableModelError) Unwrap() error { return ErrNoCapableModel }

type resolutionError struct {
	kind    error
	message string
}

func (e *resolutionError) Error() string { return e.message }
func (e *resolutionError) Unwrap() error { return e.kind }

// New validates and copies the options so later caller mutations cannot alter
// routing decisions.
func New(o Options) (*Router, error) {
	r := &Router{
		tiers:           make(map[string][]providers.ModelRef, len(o.Tiers)),
		auto:            slices.Clone(o.Auto),
		declarations:    make(map[providers.ModelRef]Declaration, len(o.Declarations)),
		difficultyTiers: make(map[string][]string, len(o.DifficultyTiers)),
		rules:           slices.Clone(o.Rules),
		classifier:      o.Classifier,
	}
	for i := range r.rules {
		if value := r.rules[i].EstimatedInputTokensGTE; value != nil {
			copy := *value
			r.rules[i].EstimatedInputTokensGTE = &copy
		}
		if value := r.rules[i].RequestedOutputTokensGTE; value != nil {
			copy := *value
			r.rules[i].RequestedOutputTokensGTE = &copy
		}
		if value := r.rules[i].ToolsPresent; value != nil {
			copy := *value
			r.rules[i].ToolsPresent = &copy
		}
	}
	for name, refs := range o.Tiers {
		if name == "" || len(refs) == 0 {
			return nil, fmt.Errorf("router: tier %q must contain at least one model", name)
		}
		for _, ref := range refs {
			if ref.Provider == "" || ref.Model == "" {
				return nil, fmt.Errorf("router: tier %q contains an invalid model reference", name)
			}
		}
		r.tiers[name] = slices.Clone(refs)
	}
	for _, name := range r.auto {
		if _, ok := r.tiers[name]; !ok {
			return nil, fmt.Errorf("router: auto names unknown tier %q", name)
		}
	}
	for label, names := range o.DifficultyTiers {
		if label != "simple" && label != "moderate" && label != "hard" {
			return nil, fmt.Errorf("router: unknown difficulty %q", label)
		}
		if len(names) == 0 {
			return nil, fmt.Errorf("router: difficulty %q must list at least one tier", label)
		}
		for _, name := range names {
			if !slices.Contains(r.auto, name) {
				return nil, fmt.Errorf("router: difficulty %q names tier %q outside auto", label, name)
			}
		}
		r.difficultyTiers[label] = slices.Clone(names)
	}
	if len(o.DifficultyTiers) > 0 || len(o.Rules) > 0 {
		for _, label := range []string{"simple", "moderate", "hard"} {
			if len(r.difficultyTiers[label]) == 0 {
				return nil, fmt.Errorf("router: difficulty %q must list at least one tier", label)
			}
		}
		if len(r.rules) > maxDifficultyRules {
			return nil, errors.New("router: rules must contain at most 32 rules")
		}
		for i, rule := range r.rules {
			if rule.Difficulty != "simple" && rule.Difficulty != "moderate" && rule.Difficulty != "hard" {
				return nil, fmt.Errorf("router: rule %d has unknown difficulty %q", i+1, rule.Difficulty)
			}
			if rule.EstimatedInputTokensGTE == nil && rule.RequestedOutputTokensGTE == nil && rule.ToolsPresent == nil {
				return nil, fmt.Errorf("router: rule %d has no conditions", i+1)
			}
			if rule.EstimatedInputTokensGTE != nil && (*rule.EstimatedInputTokensGTE < 1 || *rule.EstimatedInputTokensGTE > maxDifficultyTokenThreshold) {
				return nil, fmt.Errorf("router: rule %d has an invalid input token threshold", i+1)
			}
			if rule.RequestedOutputTokensGTE != nil && (*rule.RequestedOutputTokensGTE < 1 || *rule.RequestedOutputTokensGTE > maxDifficultyTokenThreshold) {
				return nil, fmt.Errorf("router: rule %d has an invalid output token threshold", i+1)
			}
		}
	}
	for ref, declaration := range o.Declarations {
		if ref.Provider == "" || ref.Model == "" || declaration.ContextWindow < 0 {
			return nil, fmt.Errorf("router: invalid declaration for %q", ref.String())
		}
		seen := make(map[Capability]bool, len(declaration.Capabilities))
		for _, capability := range declaration.Capabilities {
			if !knownCapability(capability) || seen[capability] {
				return nil, fmt.Errorf("router: invalid capability declaration for %q", ref.String())
			}
			seen[capability] = true
		}
		declaration.Capabilities = slices.Clone(declaration.Capabilities)
		r.declarations[ref] = declaration
	}
	return r, nil
}

func knownCapability(c Capability) bool {
	for _, known := range capabilityOrder {
		if c == known {
			return true
		}
	}
	return false
}

// Plan resolves a virtual model for one raw chat request body. ok is false
// for direct model references, which must pass through without inspection.
func (r *Router) Plan(model string, body []byte) (plan Plan, ok bool, err error) {
	return r.PlanContext(context.Background(), model, body)
}

// PlanContext resolves a virtual model and honors cancellation during local classification.
func (r *Router) PlanContext(ctx context.Context, model string, body []byte) (plan Plan, ok bool, err error) {
	var tierNames []string
	useRules := false
	switch {
	case model == "auto":
		if len(r.auto) == 0 {
			return Plan{}, true, &resolutionError{kind: ErrAutoNotConfigured, message: `auto is not configured; set router.auto`}
		}
		plan.Source = "auto"
		tierNames = r.auto
		useRules = len(r.difficultyTiers) > 0 || len(r.rules) > 0
	case strings.HasPrefix(model, "spillway/"):
		name := strings.TrimPrefix(model, "spillway/")
		if _, exists := r.tiers[name]; !exists {
			return Plan{}, true, &resolutionError{kind: ErrUnknownTier, message: unknownTierMessage(name, r.tiers)}
		}
		plan.Source = "tier"
		tierNames = []string{name}
	default:
		return Plan{}, false, nil
	}

	requirements, err := Detect(body)
	if err != nil {
		return Plan{}, true, err
	}
	if useRules {
		plan = r.difficultyPlan(requirements)
		if r.classifier != nil {
			difficulty, classifyErr := "", ctx.Err()
			if classifyErr == nil {
				difficulty, classifyErr = r.classifier.Classify(ctx, body)
			}
			if classifyErr == nil && validDifficulty(difficulty) {
				plan.Mode = "classifier"
				plan.Rule = 0
				plan.Difficulty = difficulty
			} else {
				plan.ClassifierFallback = true
			}
		}
		tierNames = r.difficultyTiers[plan.Difficulty]
		plan.SelectedTier = tierNames[0]
	}
	plan.Needs = slices.Clone(requirements.Needs)

	seen := make(map[providers.ModelRef]bool)
	var all []candidate
	for _, tier := range tierNames {
		for _, ref := range r.tiers[tier] {
			if seen[ref] {
				continue
			}
			seen[ref] = true
			all = append(all, candidate{tier: tier, ref: ref})
		}
	}

	for _, candidate := range all {
		declaration, declared := r.declarations[candidate.ref]
		if declared && len(plan.Needs) > 0 {
			missing := missingCapabilities(plan.Needs, declaration.Capabilities)
			if len(missing) > 0 {
				plan.Skipped = append(plan.Skipped, Skip{Ref: candidate.ref, Reason: "lacks " + joinCapabilities(missing)})
				continue
			}
		} else if !declared && plan.Source == "auto" && len(plan.Needs) > 0 {
			plan.Skipped = append(plan.Skipped, Skip{Ref: candidate.ref, Reason: "capabilities not declared"})
			continue
		}
		if declared && declaration.ContextWindow > 0 && requirements.EstimatedTokens > declaration.ContextWindow {
			plan.Skipped = append(plan.Skipped, Skip{
				Ref: candidate.ref, Reason: fmt.Sprintf("context too small (est. %d > %d)", requirements.EstimatedTokens, declaration.ContextWindow), size: true,
			})
			continue
		}
		plan.Eligible = append(plan.Eligible, candidate.ref)
		plan.Tiers = append(plan.Tiers, candidate.tier)
	}
	if len(plan.Eligible) > 0 {
		return plan, true, nil
	}
	return plan, true, noCandidateError(model, tierNames, requirements.EstimatedTokens, plan)
}

func validDifficulty(label string) bool {
	return label == "simple" || label == "moderate" || label == "hard"
}

func (r *Router) difficultyPlan(requirements Requirements) Plan {
	plan := Plan{Source: "auto", Mode: "rules", Difficulty: "moderate"}
	for i, rule := range r.rules {
		if rule.matches(requirements) {
			plan.Rule = i + 1
			plan.Difficulty = rule.Difficulty
			break
		}
	}
	return plan
}

func (r Rule) matches(features Requirements) bool {
	return (r.EstimatedInputTokensGTE == nil || features.EstimatedInputTokens >= *r.EstimatedInputTokensGTE) &&
		(r.RequestedOutputTokensGTE == nil || features.RequestedOutputTokens >= *r.RequestedOutputTokensGTE) &&
		(r.ToolsPresent == nil || features.ToolsPresent == *r.ToolsPresent)
}

func missingCapabilities(needs, declared []Capability) []Capability {
	var missing []Capability
	for _, need := range needs {
		if !slices.Contains(declared, need) {
			missing = append(missing, need)
		}
	}
	return missing
}

func joinCapabilities(capabilities []Capability) string {
	parts := make([]string, len(capabilities))
	for i, capability := range capabilities {
		parts[i] = string(capability)
	}
	return strings.Join(parts, ", ")
}

func unknownTierMessage(name string, tiers map[string][]providers.ModelRef) string {
	names := make([]string, 0, len(tiers))
	for tier := range tiers {
		names = append(names, tier)
	}
	slices.Sort(names)
	return fmt.Sprintf("Tier %q not found. Configured tiers: %s.", name, strings.Join(names, ", "))
}

func noCandidateError(model string, tierNames []string, estimated int, plan Plan) error {
	skipped := make([]string, len(plan.Skipped))
	allSize := len(plan.Skipped) > 0
	for i, candidate := range plan.Skipped {
		skipped[i] = candidate.Ref.String() + " " + candidate.Reason
		allSize = allSize && candidate.size
	}
	if allSize {
		return &NoCapableModelError{
			message: fmt.Sprintf("No model for %q has a context window large enough for this request (est. %d tokens): %s.",
				model, estimated, strings.Join(skipped, "; ")),
			Skipped: slices.Clone(plan.Skipped), EverySkipWasSize: true,
		}
	}
	needs := joinCapabilities(plan.Needs)
	var message string
	if plan.Source == "tier" {
		message = fmt.Sprintf("No model in tier %q supports this request", tierNames[0])
	} else {
		message = fmt.Sprintf("No model for %q supports this request", model)
	}
	if needs != "" {
		message += ", which needs " + needs
	}
	message += ": " + strings.Join(skipped, "; ") + ". Declare capabilities under models: (ADR 0007)."
	return &NoCapableModelError{message: message, Skipped: slices.Clone(plan.Skipped)}
}

// VirtualModels returns the configured virtual model names in stable order.
func (r *Router) VirtualModels() []string {
	names := make([]string, 0, len(r.tiers)+1)
	if len(r.auto) > 0 {
		names = append(names, "auto")
	}
	tiers := make([]string, 0, len(r.tiers))
	for name := range r.tiers {
		tiers = append(tiers, name)
	}
	slices.Sort(tiers)
	for _, name := range tiers {
		names = append(names, "spillway/"+name)
	}
	return names
}
