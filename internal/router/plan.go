package router

import (
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
}

// Router resolves virtual models into ordered candidates.
type Router struct {
	tiers        map[string][]providers.ModelRef
	auto         []string
	declarations map[providers.ModelRef]Declaration
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
	Source   string
	Tiers    []string
	Needs    []Capability
	Eligible []providers.ModelRef
	Skipped  []Skip
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
		tiers:        make(map[string][]providers.ModelRef, len(o.Tiers)),
		auto:         slices.Clone(o.Auto),
		declarations: make(map[providers.ModelRef]Declaration, len(o.Declarations)),
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
	var tierNames []string
	switch {
	case model == "auto":
		if len(r.auto) == 0 {
			return Plan{}, true, &resolutionError{kind: ErrAutoNotConfigured, message: `auto is not configured; set router.auto`}
		}
		plan.Source = "auto"
		tierNames = r.auto
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
