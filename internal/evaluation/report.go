package evaluation

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
)

// GateOptions configures a reproducible comparison against one baseline arm.
type GateOptions struct {
	Baseline   string
	Seed       int64
	Replicates int
}

const maxBootstrapReplicates = 100_000

// GateVerdict is the result of comparing a candidate to the baseline.
type GateVerdict string

const (
	VerdictPass         GateVerdict = "PASS"
	VerdictFail         GateVerdict = "FAIL"
	VerdictInconclusive GateVerdict = "INCONCLUSIVE"
)

// GateResult contains one gate estimate and its paired 95% bootstrap interval.
type GateResult struct {
	Arm        string      `json:"arm"`
	Difficulty string      `json:"difficulty"`
	Gate       string      `json:"gate"`
	Available  bool        `json:"available"`
	Estimate   float64     `json:"estimate"`
	Lower      float64     `json:"lower_95"`
	Upper      float64     `json:"upper_95"`
	Unit       string      `json:"unit"`
	Verdict    GateVerdict `json:"verdict"`
	Reason     string      `json:"reason,omitempty"`
	Prompts    int         `json:"prompt_clusters"`
}

// ArmCostSummary reports cost-source shares in one arm/difficulty group.
type ArmCostSummary struct {
	Arm              string             `json:"arm"`
	Difficulty       string             `json:"difficulty"`
	Requests         int                `json:"requests"`
	KnownCostShare   float64            `json:"known_cost_share"`
	CostSourceShares map[string]float64 `json:"cost_source_shares"`
}

// RaterAgreement summarizes agreement over answers rated by multiple raters.
type RaterAgreement struct {
	Raters                 []string `json:"raters"`
	PairedRatings          int      `json:"paired_ratings"`
	ExactAgreement         float64  `json:"exact_agreement"`
	QuadraticWeightedKappa float64  `json:"quadratic_weighted_kappa"`
	Available              bool     `json:"available"`
}

// GateReport is the reproducible, machine-readable output of eval-report.
type GateReport struct {
	Baseline   string           `json:"baseline"`
	Seed       int64            `json:"seed"`
	Replicates int              `json:"replicates"`
	Results    []GateResult     `json:"results"`
	Cost       []ArmCostSummary `json:"cost"`
	Agreement  RaterAgreement   `json:"inter_rater_agreement"`
}

type promptCluster struct {
	difficulty string
	trials     map[string][]Trial
	scores     map[string][]float64
	severe     map[string][]bool
}

// BuildGateReport calculates ADR 0014 gates using paired prompt-cluster resampling.
func BuildGateReport(ctx context.Context, run Report, assignments []Assignment, scoreSheets [][]Score, options GateOptions) (GateReport, error) {
	if err := ctx.Err(); err != nil {
		return GateReport{}, err
	}
	if options.Baseline == "" {
		return GateReport{}, errors.New("evaluation baseline is required")
	}
	if options.Replicates < 100 {
		return GateReport{}, errors.New("bootstrap replicates must be at least 100")
	}
	if options.Replicates > maxBootstrapReplicates {
		return GateReport{}, fmt.Errorf("bootstrap replicates must not exceed %d", maxBootstrapReplicates)
	}
	clusters, arms, err := makeClusters(run.Trials, assignments, scoreSheets)
	if err != nil {
		return GateReport{}, err
	}
	if !arms[options.Baseline] {
		return GateReport{}, fmt.Errorf("baseline arm %q is not present in the run", options.Baseline)
	}
	armNames := sortedKeys(arms)
	difficulties := []string{"all"}
	seenDifficulty := map[string]bool{}
	for _, id := range sortedKeys(clusters) {
		cluster := clusters[id]
		if !seenDifficulty[cluster.difficulty] {
			seenDifficulty[cluster.difficulty] = true
			difficulties = append(difficulties, cluster.difficulty)
		}
	}
	sort.Strings(difficulties[1:])
	result := GateReport{Baseline: options.Baseline, Seed: options.Seed, Replicates: options.Replicates, Agreement: agreement(scoreSheets)}
	gates := gateNames()
	result.Cost = summarizeCost(clusters, armNames, difficulties)
	for _, difficulty := range difficulties {
		if err := ctx.Err(); err != nil {
			return GateReport{}, err
		}
		selected := selectClusters(clusters, difficulty)
		for _, arm := range armNames {
			if arm == options.Baseline {
				continue
			}
			for gateIndex, gate := range gates {
				gateResult, err := calculateGate(ctx, selected, difficulty, options, arm, gate, gateIndex)
				if err != nil {
					return GateReport{}, err
				}
				result.Results = append(result.Results, gateResult)
			}
		}
	}
	return result, nil
}

func gateNames() [5]string {
	return [...]string{"cost_savings", "latency_p95_regression", "quality_difference", "severe_failure_difference", "known_cost_coverage"}
}

func makeClusters(trials []Trial, assignments []Assignment, sheets [][]Score) (map[string]*promptCluster, map[string]bool, error) {
	clusters := make(map[string]*promptCluster)
	arms := make(map[string]bool)
	trialByAnswer := make(map[string]Trial, len(trials))
	for _, trial := range trials {
		if trial.AnswerID == "" || trial.PromptID == "" || trial.Arm == "" || trial.Difficulty == "" {
			return nil, nil, errors.New("run contains a trial with a missing prompt, arm, or difficulty")
		}
		if trial.LatencyMS < 0 || trial.CostUSD < 0 {
			return nil, nil, fmt.Errorf("answer %q has a negative latency or cost", trial.AnswerID)
		}
		if _, exists := trialByAnswer[trial.AnswerID]; exists {
			return nil, nil, fmt.Errorf("run repeats answer %q", trial.AnswerID)
		}
		trialByAnswer[trial.AnswerID] = trial
		cluster := clusters[trial.PromptID]
		if cluster == nil {
			cluster = &promptCluster{difficulty: trial.Difficulty, trials: make(map[string][]Trial), scores: make(map[string][]float64), severe: make(map[string][]bool)}
			clusters[trial.PromptID] = cluster
		} else if cluster.difficulty != trial.Difficulty {
			return nil, nil, fmt.Errorf("prompt %q has conflicting difficulty labels", trial.PromptID)
		}
		if trial.CostSource != "reported" && trial.CostSource != "declared" && trial.CostSource != "estimated" && trial.CostSource != "unknown" && trial.CostSource != "free" {
			return nil, nil, fmt.Errorf("prompt %q has unsupported cost source %q", trial.PromptID, trial.CostSource)
		}
		cluster.trials[trial.Arm] = append(cluster.trials[trial.Arm], trial)
		arms[trial.Arm] = true
	}
	if len(clusters) == 0 {
		return nil, nil, errors.New("run has no trials")
	}
	for prompt, cluster := range clusters {
		for arm := range arms {
			if len(cluster.trials[arm]) == 0 {
				return nil, nil, fmt.Errorf("prompt %q is missing arm %q", prompt, arm)
			}
		}
	}
	assignmentByID := make(map[string]Assignment, len(assignments))
	for _, assignment := range assignments {
		if assignment.AnswerID == "" || assignment.PromptID == "" || assignment.Arm == "" {
			return nil, nil, errors.New("assignment key contains an incomplete row")
		}
		if _, exists := assignmentByID[assignment.AnswerID]; exists {
			return nil, nil, fmt.Errorf("assignment key repeats answer %q", assignment.AnswerID)
		}
		trial, exists := trialByAnswer[assignment.AnswerID]
		if !exists || trial.PromptID != assignment.PromptID || trial.Arm != assignment.Arm || trial.Repeat != assignment.Repeat {
			return nil, nil, fmt.Errorf("assignment for answer %q does not match the run", assignment.AnswerID)
		}
		assignmentByID[assignment.AnswerID] = assignment
	}
	if len(assignmentByID) != len(trialByAnswer) {
		return nil, nil, fmt.Errorf("assignment key contains %d of %d trial answers", len(assignmentByID), len(trialByAnswer))
	}
	seenRatings := make(map[string]bool)
	ratingsByAnswer := make(map[string][]Score)
	for sheetIndex, sheet := range sheets {
		for _, score := range sheet {
			assignment, ok := assignmentByID[score.AnswerID]
			if !ok || !arms[assignment.Arm] {
				return nil, nil, fmt.Errorf("score refers to unknown answer %q", score.AnswerID)
			}
			rater := score.RaterID
			if rater == "" {
				rater = fmt.Sprintf("rater-%d", sheetIndex+1)
			}
			key := rater + "\x00" + score.AnswerID
			if seenRatings[key] {
				return nil, nil, fmt.Errorf("rater %q scored answer %q more than once", rater, score.AnswerID)
			}
			seenRatings[key] = true
			if clusters[assignment.PromptID] == nil {
				return nil, nil, fmt.Errorf("score answer %q has no trial prompt", score.AnswerID)
			}
			ratingsByAnswer[score.AnswerID] = append(ratingsByAnswer[score.AnswerID], score)
		}
	}
	for answerID, ratings := range ratingsByAnswer {
		assignment := assignmentByID[answerID]
		cluster := clusters[assignment.PromptID]
		composite := make([]float64, 0, len(ratings))
		severeOutcome := ratings[0].SevereCorrectnessFault
		for _, score := range ratings {
			if score.SevereCorrectnessFault != severeOutcome {
				return nil, nil, fmt.Errorf("answer %q has conflicting severe-failure ratings; adjudicate the outcome before reporting", answerID)
			}
			composite = append(composite, (float64(score.Correctness)+float64(score.Completeness)+float64(score.InstructionFollowing))/3)
		}
		cluster.scores[assignment.Arm] = append(cluster.scores[assignment.Arm], mean(composite))
		cluster.severe[assignment.Arm] = append(cluster.severe[assignment.Arm], severeOutcome)
	}
	return clusters, arms, nil
}

func calculateGate(ctx context.Context, clusters map[string]*promptCluster, difficulty string, options GateOptions, arm, gate string, gateIndex int) (GateResult, error) {
	result := GateResult{Arm: arm, Difficulty: difficulty, Gate: gate, Unit: "%", Prompts: len(clusters), Verdict: VerdictInconclusive}
	switch gate {
	case "quality_difference":
		result.Unit = "points"
	case "severe_failure_difference":
		result.Unit = "percentage points"
	}
	if len(clusters) == 0 {
		result.Reason = "no paired prompt clusters"
		return result, nil
	}
	baseline := options.Baseline
	if gateIndex == 2 || gateIndex == 3 {
		for _, cluster := range clusters {
			if len(cluster.scores[baseline]) == 0 || len(cluster.scores[arm]) == 0 {
				result.Reason = "one or more prompt clusters lack paired ratings"
				return result, nil
			}
		}
	}
	point, valid := gateEstimate(clusters, baseline, arm, gateIndex)
	if !valid {
		result.Reason = "required paired data are unavailable or baseline cost is zero"
		return result, nil
	}
	result.Estimate = point
	ids := sortedKeys(clusters)
	values := make([]float64, 0, options.Replicates)
	// #nosec G404 -- deterministic bootstrap sampling is not used for security.
	rng := rand.New(rand.NewSource(options.Seed))
	for replicate := 0; replicate < options.Replicates; replicate++ {
		if err := ctx.Err(); err != nil {
			return GateResult{}, err
		}
		sample := make(map[string]*promptCluster, len(ids))
		for range ids {
			id := ids[rng.Intn(len(ids))]
			for occurrence := 0; ; occurrence++ {
				key := fmt.Sprintf("%s#%d", id, occurrence)
				if _, exists := sample[key]; !exists {
					sample[key] = clusters[id]
					break
				}
			}
		}
		value, ok := gateEstimate(sample, baseline, arm, gateIndex)
		if ok && !math.IsNaN(value) && !math.IsInf(value, 0) {
			values = append(values, value)
		}
	}
	if len(values) != options.Replicates {
		result.Reason = "bootstrap produced incomplete paired samples"
		return result, nil
	}
	sort.Float64s(values)
	result.Lower = values[int(.025*float64(len(values)))]
	result.Upper = values[int(.975*float64(len(values)))]
	result.Available = true
	result.Verdict, result.Reason = verdictFor(gate, point, result.Lower, result.Upper)
	return result, nil
}

func gateEstimate(clusters map[string]*promptCluster, baseline, arm string, gate int) (float64, bool) {
	baseTrials, modeTrials := make([]Trial, 0), make([]Trial, 0)
	baseScores, modeScores := make([]float64, 0), make([]float64, 0)
	baseSevere, modeSevere := 0, 0
	baseScoreCount, modeScoreCount, baseSevereN, modeSevereN := 0, 0, 0, 0
	baseCostKnown, baseCostN, modeCostKnown, modeCostN := 0, 0, 0, 0
	for _, id := range sortedKeys(clusters) {
		cluster := clusters[id]
		baseTrials = append(baseTrials, cluster.trials[baseline]...)
		modeTrials = append(modeTrials, cluster.trials[arm]...)
		baseScores = append(baseScores, cluster.scores[baseline]...)
		modeScores = append(modeScores, cluster.scores[arm]...)
		for _, v := range cluster.severe[baseline] {
			baseSevereN++
			if v {
				baseSevere++
			}
		}
		for _, v := range cluster.severe[arm] {
			modeSevereN++
			if v {
				modeSevere++
			}
		}
		baseScoreCount += len(cluster.scores[baseline])
		modeScoreCount += len(cluster.scores[arm])
		for _, trial := range cluster.trials[baseline] {
			baseCostN++
			if trial.CostSource != "unknown" {
				baseCostKnown++
			}
		}
		for _, trial := range cluster.trials[arm] {
			modeCostN++
			if trial.CostSource != "unknown" {
				modeCostKnown++
			}
		}
	}
	switch gate {
	case 0:
		base, baseOK := meanKnownCost(baseTrials)
		mode, modeOK := meanKnownCost(modeTrials)
		if !baseOK || !modeOK || base == 0 {
			return 0, false
		}
		return (base - mode) / base * 100, true
	case 1:
		base, mode := trialLatencies(baseTrials), trialLatencies(modeTrials)
		if len(base) == 0 || len(mode) == 0 {
			return 0, false
		}
		baseP95, modeP95 := percentile(base, .95), percentile(mode, .95)
		if baseP95 == 0 {
			return 0, false
		}
		return (float64(modeP95)/float64(baseP95) - 1) * 100, true
	case 2:
		if baseScoreCount == 0 || modeScoreCount == 0 {
			return 0, false
		}
		return mean(modeScores) - mean(baseScores), true
	case 3:
		if baseSevereN == 0 || modeSevereN == 0 {
			return 0, false
		}
		return (float64(modeSevere)/float64(modeSevereN) - float64(baseSevere)/float64(baseSevereN)) * 100, true
	case 4:
		if baseCostN == 0 || modeCostN == 0 {
			return 0, false
		}
		return math.Min(float64(baseCostKnown)/float64(baseCostN), float64(modeCostKnown)/float64(modeCostN)) * 100, true
	default:
		return 0, false
	}
}

func verdictFor(gate string, estimate, lower, upper float64) (GateVerdict, string) {
	switch gate {
	case "cost_savings":
		if estimate < 10 {
			return VerdictFail, "estimated savings are below the 10% requirement"
		}
		if lower > 0 {
			return VerdictPass, "savings interval is above zero and estimate meets 10%"
		}
		if upper <= 0 {
			return VerdictFail, "savings interval does not exceed zero"
		}
		return VerdictInconclusive, "savings interval includes zero"
	case "latency_p95_regression":
		if upper <= 5 {
			return VerdictPass, "upper interval bound is at most 5%"
		}
		if lower > 5 {
			return VerdictFail, "lower interval bound exceeds 5%"
		}
		return VerdictInconclusive, "latency interval crosses the 5% limit"
	case "quality_difference":
		if lower > -.2 {
			return VerdictPass, "lower interval bound is above -0.2 points"
		}
		if upper < -.2 {
			return VerdictFail, "upper interval bound is below -0.2 points"
		}
		return VerdictInconclusive, "quality interval crosses the -0.2 point limit"
	case "severe_failure_difference":
		if upper <= 2 {
			return VerdictPass, "upper interval bound is at most 2 percentage points"
		}
		if lower > 2 {
			return VerdictFail, "lower interval bound exceeds 2 percentage points"
		}
		return VerdictInconclusive, "severe-failure interval crosses the 2 percentage-point limit"
	case "known_cost_coverage":
		if estimate >= 95 {
			return VerdictPass, "both arms have at least 95% known cost coverage"
		}
		return VerdictFail, "one or both arms have less than 95% known cost coverage"
	default:
		return VerdictInconclusive, "unknown gate"
	}
}

func summarizeCost(clusters map[string]*promptCluster, arms []string, difficulties []string) []ArmCostSummary {
	var result []ArmCostSummary
	for _, difficulty := range difficulties {
		selected := selectClusters(clusters, difficulty)
		for _, arm := range arms {
			shares := map[string]float64{"reported": 0, "declared-price": 0, "estimated-token": 0, "free": 0, "unknown": 0}
			known, total := 0, 0
			for _, cluster := range selected {
				for _, trial := range cluster.trials[arm] {
					total++
					switch trial.CostSource {
					case "reported":
						shares["reported"]++
					case "declared":
						shares["declared-price"]++
						known++
					case "free":
						shares["free"]++
						known++
					case "estimated":
						shares["estimated-token"]++
						known++
					case "unknown":
						shares["unknown"]++
					}
					if trial.CostSource == "reported" {
						known++
					}
				}
			}
			if total > 0 {
				for key := range shares {
					shares[key] /= float64(total)
				}
			}
			result = append(result, ArmCostSummary{Arm: arm, Difficulty: difficulty, Requests: total, KnownCostShare: ratio(known, total), CostSourceShares: shares})
		}
	}
	return result
}

func agreement(sheets [][]Score) RaterAgreement {
	byRater := make(map[string]map[string]int)
	for i, sheet := range sheets {
		defaultID := fmt.Sprintf("rater-%d", i+1)
		for _, score := range sheet {
			rater := score.RaterID
			if rater == "" {
				rater = defaultID
			}
			if byRater[rater] == nil {
				byRater[rater] = make(map[string]int)
			}
			composite := float64(score.Correctness+score.Completeness+score.InstructionFollowing) / 3
			byRater[rater][score.AnswerID] = int(math.Round(composite))
		}
	}
	raters := sortedKeys(byRater)
	result := RaterAgreement{Raters: raters}
	if len(raters) < 2 {
		return result
	}
	pairs, exact, observed := 0, 0, 0.0
	counts := [5][5]float64{}
	for i := 0; i < len(raters); i++ {
		for j := i + 1; j < len(raters); j++ {
			left, right := byRater[raters[i]], byRater[raters[j]]
			for id, a := range left {
				b, ok := right[id]
				if !ok {
					continue
				}
				pairs++
				if a == b {
					exact++
				}
				counts[a-1][b-1]++
				observed += math.Pow(float64(a-b), 2)
			}
		}
	}
	result.PairedRatings = pairs
	if pairs == 0 {
		return result
	}
	result.Available = true
	result.ExactAgreement = float64(exact) / float64(pairs)
	// Use the two-rater pooled marginals for the chance disagreement baseline.
	leftMarginal, rightMarginal := [5]float64{}, [5]float64{}
	for i := 0; i < 5; i++ {
		for j := 0; j < 5; j++ {
			leftMarginal[i] += counts[i][j]
			rightMarginal[j] += counts[i][j]
		}
	}
	expected := 0.0
	for i := 0; i < 5; i++ {
		for j := 0; j < 5; j++ {
			expected += leftMarginal[i] * rightMarginal[j] * math.Pow(float64(i-j), 2) / float64(pairs)
		}
	}
	if expected > 0 {
		result.QuadraticWeightedKappa = 1 - observed/expected
	} else if observed == 0 {
		result.QuadraticWeightedKappa = 1
	}
	return result
}

func meanKnownCost(trials []Trial) (float64, bool) {
	total, n := 0.0, 0
	for _, trial := range trials {
		if trial.CostSource != "unknown" {
			total += trial.CostUSD
			n++
		}
	}
	if n == 0 {
		return 0, false
	}
	return total / float64(n), true
}
func trialLatencies(trials []Trial) []int64 {
	out := make([]int64, 0, len(trials))
	for _, trial := range trials {
		out = append(out, trial.LatencyMS)
	}
	return out
}
func mean(values []float64) float64 {
	total := 0.0
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}
func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}
func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
func selectClusters(clusters map[string]*promptCluster, difficulty string) map[string]*promptCluster {
	selected := make(map[string]*promptCluster)
	for id, cluster := range clusters {
		if difficulty == "all" || cluster.difficulty == difficulty {
			selected[id] = cluster
		}
	}
	return selected
}
