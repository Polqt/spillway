package evaluation_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Polqt/spillway/internal/evaluation"
)

func TestLoadPromptSet_V2Corpus(t *testing.T) {
	file, err := os.Open(filepath.Join("..", "..", "examples", "evaluation-prompts.v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Errorf("close prompt corpus: %v", err)
		}
	})
	set, err := evaluation.LoadPromptSet(file)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, prompt := range set.Prompts {
		counts[prompt.Difficulty]++
		if len(prompt.Settings["max_tokens"]) == 0 {
			t.Errorf("prompt %s has no max_tokens", prompt.ID)
		}
	}
	for _, difficulty := range []string{"simple", "moderate", "hard"} {
		if counts[difficulty] != 30 {
			t.Errorf("%s prompts = %d, want 30", difficulty, counts[difficulty])
		}
	}
}

func TestLoadScores_MultipleRatersAndLegacySheet(t *testing.T) {
	withRaters, err := evaluation.LoadScores(strings.NewReader("rater_id,answer_id,correctness_1_to_5,completeness_1_to_5,instruction_following_1_to_5,severe_correctness_failure,adjudication_note\nr1,a1,4,4,5,false,\nr2,a1,5,4,4,false,\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(withRaters) != 2 || withRaters[0].RaterID != "r1" || withRaters[1].RaterID != "r2" {
		t.Fatalf("ratings = %#v", withRaters)
	}
	legacy, err := evaluation.LoadScores(strings.NewReader("answer_id,correctness_1_to_5,completeness_1_to_5,instruction_following_1_to_5,severe_correctness_failure,adjudication_note\na1,4,4,5,false,\n"))
	if err != nil || len(legacy) != 1 || legacy[0].RaterID != "" {
		t.Fatalf("legacy ratings = %#v, err %v", legacy, err)
	}
}

func TestBuildGateReportValidatesContextAndReplicateLimit(t *testing.T) {
	t.Run("canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := evaluation.BuildGateReport(ctx, evaluation.Report{}, nil, nil, evaluation.GateOptions{Baseline: "baseline", Replicates: 100})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("BuildGateReport error = %v, want context canceled", err)
		}
	})

	t.Run("replicate limit", func(t *testing.T) {
		parent, cancel := context.WithCancel(t.Context())
		defer cancel()
		ctx := &cancelAfterChecksContext{Context: parent, cancel: cancel, remaining: 1}
		trials, assignments := pairedTrialData()
		_, err := evaluation.BuildGateReport(ctx, evaluation.Report{Trials: trials}, assignments, nil, evaluation.GateOptions{Baseline: "baseline", Replicates: 100_001})
		if !errors.Is(err, evaluation.ErrReplicateLimit) {
			t.Fatalf("BuildGateReport error = %v, want replicate limit error", err)
		}
	})
}

func TestBuildGateReportStopsDuringBootstrap(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &cancelAfterChecksContext{Context: parent, cancel: cancel, remaining: 2}
	trials, assignments := pairedTrialData()
	_, err := evaluation.BuildGateReport(ctx, evaluation.Report{Trials: trials}, assignments, nil, evaluation.GateOptions{Baseline: "baseline", Replicates: 100})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("BuildGateReport error = %v, want context canceled during bootstrap", err)
	}
}

func pairedTrialData() ([]evaluation.Trial, []evaluation.Assignment) {
	trials := []evaluation.Trial{
		{AnswerID: "base", PromptID: "prompt", Difficulty: "simple", Arm: "baseline", LatencyMS: 10, CostUSD: .02, CostSource: "reported", Outcome: evaluation.OutcomeSucceeded},
		{AnswerID: "candidate", PromptID: "prompt", Difficulty: "simple", Arm: "candidate", LatencyMS: 10, CostUSD: .01, CostSource: "reported", Outcome: evaluation.OutcomeSucceeded},
	}
	assignments := []evaluation.Assignment{
		{AnswerID: "base", PromptID: "prompt", Arm: "baseline"},
		{AnswerID: "candidate", PromptID: "prompt", Arm: "candidate"},
	}
	return trials, assignments
}

type cancelAfterChecksContext struct {
	context.Context
	cancel    context.CancelFunc
	remaining int
}

func (c *cancelAfterChecksContext) Err() error {
	if c.remaining == 0 {
		c.cancel()
	}
	c.remaining--
	return c.Context.Err()
}

func TestReport_IdenticalArmsGiveZeroIntervals(t *testing.T) {
	trials := []evaluation.Trial{}
	assignments := []evaluation.Assignment{}
	scores := []evaluation.Score{}
	for p := 0; p < 8; p++ {
		id := string(rune('a' + p))
		for _, arm := range []string{"current-auto", "rules"} {
			answerID := id + arm
			trials = append(trials, evaluation.Trial{PromptID: id, Difficulty: "simple", Arm: arm, Repeat: 1, LatencyMS: 100, CostUSD: .01, CostSource: "reported", Outcome: evaluation.OutcomeSucceeded, AnswerID: answerID})
			assignments = append(assignments, evaluation.Assignment{AnswerID: answerID, PromptID: id, Arm: arm, Repeat: 1})
			scores = append(scores, evaluation.Score{AnswerID: answerID, RaterID: "r1", Correctness: 4, Completeness: 4, InstructionFollowing: 4})
		}
	}
	got, err := evaluation.BuildGateReport(t.Context(), evaluation.Report{Trials: trials}, assignments, [][]evaluation.Score{scores}, evaluation.GateOptions{Baseline: "current-auto", Seed: 12, Replicates: 300})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != 12 {
		t.Fatalf("got %d gate results, want 12 across aggregate and simple difficulty", len(got.Results))
	}
	for _, gate := range got.Results {
		if gate.Arm != "rules" {
			t.Errorf("unexpected arm in gate result: %s", gate.Arm)
			continue
		}
		if !gate.Available {
			t.Errorf("%s/%s result unavailable", gate.Difficulty, gate.Gate)
		}
		wantEstimate := 0.0
		if gate.Gate == "known_cost_coverage" {
			wantEstimate = 100
		}
		if gate.Estimate != wantEstimate {
			t.Errorf("%s/%s estimate = %v, want %v", gate.Difficulty, gate.Gate, gate.Estimate, wantEstimate)
		}
		if gate.Lower != wantEstimate || gate.Upper != wantEstimate {
			t.Errorf("%s/%s interval = [%v,%v], want [%v,%v]", gate.Difficulty, gate.Gate, gate.Lower, gate.Upper, wantEstimate, wantEstimate)
		}
	}
}

func TestReport_RequiresIndependentRatingsAndIncludesSuccessGate(t *testing.T) {
	trials := []evaluation.Trial{
		{AnswerID: "base", PromptID: "prompt-1", Category: "code", Difficulty: "simple", Arm: "auto", Repeat: 1, CostUSD: .01, CostSource: evaluation.CostReported, Outcome: evaluation.OutcomeSucceeded, RatedForReview: true},
		{AnswerID: "candidate", PromptID: "prompt-1", Category: "code", Difficulty: "simple", Arm: "rules", Repeat: 1, CostUSD: .009, CostSource: evaluation.CostReported, Outcome: evaluation.OutcomeFailed, RatedForReview: true},
	}
	assignments := []evaluation.Assignment{
		{AnswerID: "base", PromptID: "prompt-1", Arm: "auto", Repeat: 1, RatedForReview: true},
		{AnswerID: "candidate", PromptID: "prompt-1", Arm: "rules", Repeat: 1, RatedForReview: true},
	}
	baseScores := []evaluation.Score{{AnswerID: "base", RaterID: "rater-a", Correctness: 4, Completeness: 4, InstructionFollowing: 4}}
	if _, err := evaluation.BuildGateReport(t.Context(), evaluation.Report{FormatVersion: 2, Trials: trials}, assignments, [][]evaluation.Score{baseScores}, evaluation.GateOptions{Baseline: "auto", Replicates: 10000}); err == nil || !strings.Contains(err.Error(), "exactly two") {
		t.Fatalf("single-rater report error = %v, want independent-rater validation", err)
	}
	candidateScores := []evaluation.Score{
		{AnswerID: "base", RaterID: "rater-b", Correctness: 4, Completeness: 4, InstructionFollowing: 4},
	}
	got, err := evaluation.BuildGateReport(t.Context(), evaluation.Report{FormatVersion: 2, Trials: trials}, assignments, [][]evaluation.Score{baseScores, candidateScores}, evaluation.GateOptions{Baseline: "auto", Seed: 3, Replicates: 10000})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	foundCategory := false
	for _, gate := range got.Results {
		if gate.Gate == "request_success_difference" && gate.Difficulty == "all" {
			if gate.Category == "all" {
				found = true
			}
			if gate.Category == "code" {
				foundCategory = true
			}
			if gate.Estimate != -100 || gate.Unit != "percentage points" {
				t.Errorf("request success gate = %#v, want -100 percentage points", gate)
			}
		}
	}
	if !found {
		t.Fatal("report does not contain the ADR 0023 request-success gate")
	}
	if !foundCategory {
		t.Fatal("report does not contain category-level gates")
	}
}

func TestReport_RejectsMissingPreselectedRatings(t *testing.T) {
	trials := []evaluation.Trial{
		{AnswerID: "base", PromptID: "prompt-1", Category: "code", Difficulty: "simple", Arm: "auto", Repeat: 1, CostSource: evaluation.CostReported, Outcome: evaluation.OutcomeSucceeded, RatedForReview: true},
		{AnswerID: "rules", PromptID: "prompt-1", Category: "code", Difficulty: "simple", Arm: "rules", Repeat: 1, CostSource: evaluation.CostReported, Outcome: evaluation.OutcomeSucceeded, RatedForReview: true},
	}
	assignments := []evaluation.Assignment{
		{AnswerID: "base", PromptID: "prompt-1", Arm: "auto", Repeat: 1, RatedForReview: true},
		{AnswerID: "rules", PromptID: "prompt-1", Arm: "rules", Repeat: 1, RatedForReview: true},
	}
	sheets := [][]evaluation.Score{
		{{AnswerID: "base", RaterID: "rater-a", Correctness: 4, Completeness: 4, InstructionFollowing: 4}},
		{{AnswerID: "base", RaterID: "rater-b", Correctness: 4, Completeness: 4, InstructionFollowing: 4}},
	}
	_, err := evaluation.BuildGateReport(t.Context(), evaluation.Report{FormatVersion: 3, Trials: trials}, assignments, sheets, evaluation.GateOptions{Baseline: "auto", Replicates: 10000})
	if err == nil || !strings.Contains(err.Error(), "preselected successful trial \"rules\" requires two ratings") {
		t.Fatalf("BuildGateReport error = %v, want missing preselected rating rejection", err)
	}
}

func TestReport_GateVerdictsOnConstructedData(t *testing.T) {
	// Constructed prompt clusters keep every metric far from its threshold.
	trials := []evaluation.Trial{}
	assignments := []evaluation.Assignment{}
	scores := []evaluation.Score{}
	for p := 0; p < 30; p++ {
		id := string(rune(p + 100))
		for _, arm := range []string{"current-auto", "rules", "bad", "uncertain"} {
			for repeat := 1; repeat <= 2; repeat++ {
				answerID := id + arm + string(rune('0'+repeat))
				latency, cost, score := int64(100), .02, 5
				severe := false
				switch arm {
				case "rules":
					latency, cost = 100, .01
				case "bad":
					latency, cost, score, severe = 200, .04, 1, true
				case "uncertain":
					cost = 0
				}
				costSource := "reported"
				switch arm {
				case "uncertain":
					costSource = "unknown"
				}
				trials = append(trials, evaluation.Trial{PromptID: id, Difficulty: "moderate", Arm: arm, Repeat: repeat, LatencyMS: latency, CostUSD: cost, CostSource: evaluation.CostSource(costSource), Outcome: evaluation.OutcomeSucceeded, AnswerID: answerID})
				assignments = append(assignments, evaluation.Assignment{AnswerID: answerID, PromptID: id, Arm: arm, Repeat: repeat})
				scores = append(scores, evaluation.Score{AnswerID: answerID, RaterID: "r1", Correctness: score, Completeness: score, InstructionFollowing: score, SevereCorrectnessFault: severe})
			}
		}
	}
	got, err := evaluation.BuildGateReport(t.Context(), evaluation.Report{Trials: trials}, assignments, [][]evaluation.Score{scores}, evaluation.GateOptions{Baseline: "current-auto", Seed: 9, Replicates: 1000})
	if err != nil {
		t.Fatal(err)
	}
	verdict := map[string]map[string]evaluation.GateVerdict{}
	for _, result := range got.Results {
		if result.Difficulty != "all" {
			continue
		}
		if verdict[result.Arm] == nil {
			verdict[result.Arm] = map[string]evaluation.GateVerdict{}
		}
		verdict[result.Arm][result.Gate] = result.Verdict
	}
	if verdict["rules"]["cost_savings"] != evaluation.VerdictPass || verdict["bad"]["cost_savings"] != evaluation.VerdictFail || verdict["bad"]["quality_difference"] != evaluation.VerdictFail {
		t.Fatalf("verdicts = %#v", verdict)
	}
	if verdict["rules"]["quality_difference"] != evaluation.VerdictPass || verdict["bad"]["latency_p95_regression"] != evaluation.VerdictFail {
		t.Fatalf("verdicts = %#v", verdict)
	}
	if verdict["uncertain"]["cost_savings"] != evaluation.VerdictInconclusive {
		t.Fatalf("uncertain verdicts = %#v", verdict["uncertain"])
	}
}

func TestReport_DeterministicForSeed(t *testing.T) {
	trials, assignments, scores := reportFixture()
	options := evaluation.GateOptions{Baseline: "current-auto", Seed: 77, Replicates: 250}
	first, err := evaluation.BuildGateReport(t.Context(), evaluation.Report{Trials: trials}, assignments, [][]evaluation.Score{scores}, options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := evaluation.BuildGateReport(t.Context(), evaluation.Report{Trials: trials}, assignments, [][]evaluation.Score{scores}, options)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Fatal("same seed produced different reports")
	}
}

func TestReport_ZeroBaselineCostIsInconclusive(t *testing.T) {
	trials, assignments, scores := reportFixture()
	for i := range trials {
		trials[i].CostUSD = 0
		trials[i].CostSource = "free"
	}
	got, err := evaluation.BuildGateReport(t.Context(), evaluation.Report{Trials: trials}, assignments, [][]evaluation.Score{scores}, evaluation.GateOptions{Baseline: "current-auto", Seed: 2, Replicates: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range got.Results {
		if result.Gate == "cost_savings" && result.Verdict != evaluation.VerdictInconclusive {
			t.Errorf("cost verdict = %s, want INCONCLUSIVE", result.Verdict)
		}
		if result.Gate == "cost_savings" && result.Available {
			t.Error("zero-baseline cost savings should be unavailable")
		}
	}
}

func TestReport_RecommendsLowerRuleDifficultyOnlyWithPairedEvidence(t *testing.T) {
	trials := make([]evaluation.Trial, 0, 60)
	assignments := make([]evaluation.Assignment, 0, 60)
	raterA := make([]evaluation.Score, 0, 60)
	raterB := make([]evaluation.Score, 0, 60)
	for index := 0; index < 30; index++ {
		promptID := fmt.Sprintf("prompt-%02d", index)
		for _, arm := range []string{"current-auto", "rules"} {
			answerID := promptID + "-" + arm
			trial := evaluation.Trial{
				AnswerID: answerID, PromptID: promptID, Category: "coding", Difficulty: "simple",
				Arm: arm, Repeat: 1, CostSource: evaluation.CostReported, Outcome: evaluation.OutcomeSucceeded,
				RatedForReview: true, CostUSD: .01,
			}
			if arm == "rules" {
				trial.CostUSD = .02
				trial.RouteReason = "rules; rule 1; difficulty hard; tier smart; candidate 1/1"
			}
			trials = append(trials, trial)
			assignments = append(assignments, evaluation.Assignment{AnswerID: answerID, PromptID: promptID, Arm: arm, Repeat: 1, RatedForReview: true})
			raterA = append(raterA, evaluation.Score{AnswerID: answerID, RaterID: "rater-a", Correctness: 4, Completeness: 4, InstructionFollowing: 4})
			raterB = append(raterB, evaluation.Score{AnswerID: answerID, RaterID: "rater-b", Correctness: 4, Completeness: 4, InstructionFollowing: 4})
		}
	}
	got, err := evaluation.BuildGateReport(t.Context(), evaluation.Report{FormatVersion: 3, Trials: trials}, assignments, [][]evaluation.Score{raterA, raterB}, evaluation.GateOptions{Baseline: "current-auto", Seed: 77, Replicates: 10_000})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.RoutingFindings) != 2 {
		t.Fatalf("routing findings = %d, want aggregate and coding findings: %#v", len(got.RoutingFindings), got.RoutingFindings)
	}
	for _, finding := range got.RoutingFindings {
		if finding.Pattern != "over_routing" || finding.Verdict != evaluation.VerdictPass || !strings.Contains(finding.Recommendation, "lowering") || finding.PromptClusters != 30 || finding.CostLower95USD <= 0 || finding.QualityUpper95 > 0 {
			t.Errorf("routing finding = %#v, want evidence-backed lower-difficulty recommendation", finding)
		}
	}
}

func TestReport_ReproducesV05Stage2Intervals(t *testing.T) {
	file, err := os.Open(filepath.Join("testdata", "stage2-metrics.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Errorf("close stage 2 fixture: %v", err)
		}
	})
	var report evaluation.Report
	if err := json.NewDecoder(file).Decode(&report); err != nil {
		t.Fatal(err)
	}
	assignments := []evaluation.Assignment{}
	for _, trial := range report.Trials {
		assignments = append(assignments, evaluation.Assignment{AnswerID: trial.AnswerID, PromptID: trial.PromptID, Arm: trial.Arm, Repeat: trial.Repeat})
	}
	got, err := evaluation.BuildGateReport(t.Context(), report, assignments, nil, evaluation.GateOptions{Baseline: "current-auto", Seed: 20261016, Replicates: 10000})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]float64{"rules": {0, 0}, "classifier-unavailable": {0, 50}}
	found := map[string]bool{}
	for _, result := range got.Results {
		if result.Difficulty != "all" || result.Category != "all" || result.Gate != "latency_p95_regression" {
			continue
		}
		interval, ok := want[result.Arm]
		if ok {
			found[result.Arm] = true
			if !result.Available {
				t.Errorf("%s interval unavailable", result.Arm)
			}
			if result.Lower != interval[0] || result.Upper != interval[1] {
				t.Errorf("%s interval = [%v,%v], want [%v,%v]", result.Arm, result.Lower, result.Upper, interval[0], interval[1])
			}
		}
	}
	for arm := range want {
		if !found[arm] {
			t.Errorf("missing stage 2 latency result for arm %s", arm)
		}
	}
}

func reportFixture() ([]evaluation.Trial, []evaluation.Assignment, []evaluation.Score) {
	trials := []evaluation.Trial{}
	assignments := []evaluation.Assignment{}
	scores := []evaluation.Score{}
	for p := 0; p < 12; p++ {
		id := string(rune('a' + p))
		for _, arm := range []string{"current-auto", "rules"} {
			answerID := id + arm
			trials = append(trials, evaluation.Trial{PromptID: id, Difficulty: "simple", Arm: arm, Repeat: 1, LatencyMS: int64(100 + p), CostUSD: .01, CostSource: "reported", Outcome: evaluation.OutcomeSucceeded, AnswerID: answerID})
			assignments = append(assignments, evaluation.Assignment{AnswerID: answerID, PromptID: id, Arm: arm, Repeat: 1})
			score := 4
			switch arm {
			case "rules":
				score = 5
			}
			scores = append(scores, evaluation.Score{AnswerID: answerID, RaterID: "r1", Correctness: score, Completeness: score, InstructionFollowing: score})
		}
	}
	return trials, assignments, scores
}
