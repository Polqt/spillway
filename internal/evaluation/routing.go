package evaluation

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
)

const minRoutingFindingPrompts = 30

// RoutingFinding is a content-free, paired assessment of a rules route that
// differs from the corpus difficulty label.
type RoutingFinding struct {
	Arm              string      `json:"arm"`
	Difficulty       string      `json:"corpus_difficulty"`
	RoutedDifficulty string      `json:"rules_difficulty"`
	Category         string      `json:"category"`
	Pattern          string      `json:"pattern"`
	PromptClusters   int         `json:"prompt_clusters"`
	CostDeltaUSD     float64     `json:"mean_cost_delta_usd"`
	CostLower95USD   float64     `json:"cost_delta_lower_95_usd"`
	CostUpper95USD   float64     `json:"cost_delta_upper_95_usd"`
	QualityDelta     float64     `json:"mean_quality_delta"`
	QualityLower95   float64     `json:"quality_delta_lower_95"`
	QualityUpper95   float64     `json:"quality_delta_upper_95"`
	SevereDeltaPP    float64     `json:"severe_failure_delta_percentage_points"`
	SevereLower95PP  float64     `json:"severe_delta_lower_95_percentage_points"`
	SevereUpper95PP  float64     `json:"severe_delta_upper_95_percentage_points"`
	Verdict          GateVerdict `json:"verdict"`
	Recommendation   string      `json:"config_recommendation,omitempty"`
	Reason           string      `json:"reason,omitempty"`
}

type routeEvidence struct {
	prompt        string
	costDelta     float64
	qualityDelta  float64
	severeDeltaPP float64
}

func routingFindings(ctx context.Context, run Report, clusters map[string]*promptCluster, options GateOptions) ([]RoutingFinding, error) {
	if len(run.Trials) == 0 {
		return nil, nil
	}
	groups := make(map[string][]routeEvidence)
	labels := make(map[string][4]string)
	for _, trial := range run.Trials {
		if trial.Arm != "rules" || !trial.RatedForReview || trial.Outcome != OutcomeSucceeded {
			continue
		}
		routed := rulesDifficulty(trial.RouteReason)
		if routed == "" || difficultyOrder(routed) == difficultyOrder(trial.Difficulty) {
			continue
		}
		cluster := clusters[trial.PromptID]
		if cluster == nil || len(cluster.scores[options.Baseline]) != 1 || len(cluster.scores[trial.Arm]) != 1 || len(cluster.severe[options.Baseline]) != 1 || len(cluster.severe[trial.Arm]) != 1 {
			continue
		}
		baselineCost, baselineKnown := meanKnownCost(cluster.trials[options.Baseline])
		rulesCost, rulesKnown := meanKnownCost(cluster.trials[trial.Arm])
		if !baselineKnown || !rulesKnown {
			continue
		}
		evidence := routeEvidence{
			prompt: trial.PromptID, costDelta: rulesCost - baselineCost,
			qualityDelta: cluster.scores[trial.Arm][0] - cluster.scores[options.Baseline][0],
		}
		baselineSevere, rulesSevere := 0.0, 0.0
		if cluster.severe[options.Baseline][0] {
			baselineSevere = 1
		}
		if cluster.severe[trial.Arm][0] {
			rulesSevere = 1
		}
		evidence.severeDeltaPP = (rulesSevere - baselineSevere) * 100
		categories := []string{"all"}
		if trial.Category != "" && trial.Category != "all" {
			categories = append(categories, trial.Category)
		}
		for _, category := range categories {
			key := strings.Join([]string{trial.Difficulty, routed, category}, "\x00")
			groups[key] = append(groups[key], evidence)
			labels[key] = [4]string{trial.Arm, trial.Difficulty, routed, category}
		}
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	findings := make([]RoutingFinding, 0, len(keys))
	for index, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		values := groups[key]
		sort.Slice(values, func(i, j int) bool { return values[i].prompt < values[j].prompt })
		arm, expected, routed, category := labels[key][0], labels[key][1], labels[key][2], labels[key][3]
		pattern := "over_routing"
		if difficultyOrder(routed) < difficultyOrder(expected) {
			pattern = "under_routing"
		}
		finding := RoutingFinding{
			Arm: arm, Difficulty: expected, RoutedDifficulty: routed, Category: category,
			Pattern: pattern, PromptClusters: len(values), Verdict: VerdictInconclusive,
		}
		if len(values) < minRoutingFindingPrompts {
			finding.Reason = fmt.Sprintf("requires at least %d paired rated prompt clusters", minRoutingFindingPrompts)
			findings = append(findings, finding)
			continue
		}
		cost, costLower, costUpper, err := bootstrapMean(ctx, routeValues(values, func(item routeEvidence) float64 { return item.costDelta }), options.Seed+int64(index)*3, options.Replicates)
		if err != nil {
			return nil, err
		}
		quality, qualityLower, qualityUpper, err := bootstrapMean(ctx, routeValues(values, func(item routeEvidence) float64 { return item.qualityDelta }), options.Seed+int64(index)*3+1, options.Replicates)
		if err != nil {
			return nil, err
		}
		severe, severeLower, severeUpper, err := bootstrapMean(ctx, routeValues(values, func(item routeEvidence) float64 { return item.severeDeltaPP }), options.Seed+int64(index)*3+2, options.Replicates)
		if err != nil {
			return nil, err
		}
		finding.CostDeltaUSD, finding.CostLower95USD, finding.CostUpper95USD = cost, costLower, costUpper
		finding.QualityDelta, finding.QualityLower95, finding.QualityUpper95 = quality, qualityLower, qualityUpper
		finding.SevereDeltaPP, finding.SevereLower95PP, finding.SevereUpper95PP = severe, severeLower, severeUpper
		switch {
		case pattern == "over_routing" && costLower > 0 && qualityUpper <= 0 && severeUpper <= 2:
			finding.Verdict = VerdictPass
			finding.Recommendation = "consider lowering the configured rule difficulty for this corpus difficulty and category"
			finding.Reason = "rules cost more without a measured quality gain or material severe-failure increase"
		case pattern == "under_routing" && (qualityUpper < 0 || severeLower > 2):
			finding.Verdict = VerdictPass
			finding.Recommendation = "consider raising the configured rule difficulty for this corpus difficulty and category"
			finding.Reason = "paired blinded ratings show a quality loss or severe-failure increase"
		default:
			finding.Reason = "paired cost and blinded-rating intervals do not support a rule change"
		}
		findings = append(findings, finding)
	}
	return findings, nil
}

func rulesDifficulty(reason string) string {
	if !strings.HasPrefix(reason, "rules;") {
		return ""
	}
	for _, part := range strings.Split(reason, ";") {
		label := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(part), "difficulty "))
		if label != "" && label != strings.TrimSpace(part) && difficultyOrder(label) > 0 {
			return label
		}
	}
	return ""
}

func difficultyOrder(value string) int {
	switch value {
	case "simple":
		return 1
	case "moderate":
		return 2
	case "hard":
		return 3
	default:
		return 0
	}
}

func routeValues(values []routeEvidence, selectValue func(routeEvidence) float64) []float64 {
	result := make([]float64, len(values))
	for i, value := range values {
		result[i] = selectValue(value)
	}
	return result
}

func bootstrapMean(ctx context.Context, samples []float64, seed int64, replicates int) (float64, float64, float64, error) {
	if len(samples) == 0 || replicates < 100 {
		return 0, 0, 0, errors.New("routing assessment requires paired samples and at least 100 replicates")
	}
	point := mean(samples)
	values := make([]float64, replicates)
	// #nosec G404 -- deterministic bootstrap sampling is not used for security.
	rng := rand.New(rand.NewSource(seed))
	for replicate := range replicates {
		if err := ctx.Err(); err != nil {
			return 0, 0, 0, err
		}
		total := 0.0
		for range samples {
			total += samples[rng.Intn(len(samples))]
		}
		values[replicate] = total / float64(len(samples))
	}
	sort.Float64s(values)
	return point, values[int(.025*float64(replicates))], values[int(.975*float64(replicates))], nil
}
