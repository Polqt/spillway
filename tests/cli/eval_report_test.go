package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Polqt/spillway/internal/cli"
	"github.com/Polqt/spillway/internal/evaluation"
)

func TestRun_EvalFileCommandsHonorCanceledContext(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "eval-score", args: []string{"eval-score", "-run", t.TempDir()}},
		{name: "eval-report", args: []string{"eval-report", "-run", t.TempDir(), "-baseline", "auto"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			var stdout, stderr bytes.Buffer
			if code := cli.Run(ctx, tc.args, nil, &stdout, &stderr, noEnv); code != 1 || !strings.Contains(stderr.String(), "context canceled") {
				t.Fatalf("exit = %d; stderr = %q; want cancellation error", code, stderr.String())
			}
		})
	}
}

func TestRun_EvalScoreReplacesQualitySummaryAtomically(t *testing.T) {
	runDir := t.TempDir()
	writeEvalScoreInputs(t, runDir)
	qualityPath := filepath.Join(runDir, "quality.json")
	if err := os.WriteFile(qualityPath, []byte(`{"old":"summary"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := cli.Run(t.Context(), []string{"eval-score", "-run", runDir}, nil, &stdout, &stderr, noEnv); code != 0 {
		t.Fatalf("exit = %d; stderr = %q", code, stderr.String())
	}
	var metrics []evaluation.QualityMetrics
	// #nosec G304 -- this path is inside the test's temporary directory.
	data, err := os.ReadFile(qualityPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &metrics); err != nil {
		t.Fatalf("quality.json is invalid JSON: %v", err)
	}
	if len(metrics) != 4 || metrics[0].Arm == "" {
		t.Fatalf("quality summary metrics = %#v, want four nonempty grouped summaries", metrics)
	}
}

func TestRun_EvalScorePreservesQualitySummaryWhenReplaceFails(t *testing.T) {
	runDir := t.TempDir()
	writeEvalScoreInputs(t, runDir)
	qualityDir := filepath.Join(runDir, "quality.json")
	if err := os.Mkdir(qualityDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinelPath := filepath.Join(qualityDir, "keep")
	if err := os.WriteFile(sentinelPath, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := cli.Run(t.Context(), []string{"eval-score", "-run", runDir}, nil, &stdout, &stderr, noEnv); code != 1 {
		t.Fatalf("exit = %d; stderr = %q, want replace failure", code, stderr.String())
	}
	// #nosec G304 -- this path is inside the test's temporary directory.
	data, err := os.ReadFile(sentinelPath)
	if err != nil || string(data) != "preserve" {
		t.Fatalf("existing quality summary sentinel = %q, err = %v", data, err)
	}
	entries, err := os.ReadDir(runDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".quality-") && strings.HasSuffix(entry.Name(), ".tmp") {
			t.Errorf("temporary summary %q left behind after failed replace", entry.Name())
		}
	}
}

func writeEvalScoreInputs(t *testing.T, runDir string) {
	t.Helper()
	files := map[string]string{
		"rater-1.jsonl":  `{"trial_id":"trial-1","prompt_id":"prompt-1","difficulty":"simple","category":"code","completion":"ok"}` + "\n",
		"blind-key.json": `[{"trial_id":"trial-1","prompt_id":"prompt-1","arm":"rules","repeat":1}]`,
		"scores.csv":     "trial_id,correctness_1_to_5,completeness_1_to_5,instruction_following_1_to_5,severe_correctness_failure,adjudication_note\ntrial-1,5,5,5,false,\n",
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(runDir, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRun_EvalReportPrintsGatesAndAgreement(t *testing.T) {
	runDir := t.TempDir()
	var trials []evaluation.Trial
	var assignments []evaluation.Assignment
	var rows strings.Builder
	rows.WriteString("rater_id,answer_id,correctness_1_to_5,completeness_1_to_5,instruction_following_1_to_5,severe_correctness_failure,adjudication_note\n")
	for prompt := 0; prompt < 8; prompt++ {
		promptID := "p" + string(rune('a'+prompt))
		for _, arm := range []string{"current-auto", "rules"} {
			answerID := promptID + "-" + arm
			cost := .02
			if arm == "rules" {
				cost = .01
			}
			trials = append(trials, evaluation.Trial{AnswerID: answerID, PromptID: promptID, Difficulty: "simple", Arm: arm, Repeat: 1, LatencyMS: 100, CostUSD: cost, CostSource: "reported", Outcome: evaluation.OutcomeSucceeded})
			assignments = append(assignments, evaluation.Assignment{AnswerID: answerID, PromptID: promptID, Arm: arm, Repeat: 1})
			for _, rater := range []string{"rater-a", "rater-b"} {
				rows.WriteString(strings.Join([]string{rater, answerID, "4", "4", "4", "false", ""}, ","))
				rows.WriteByte('\n')
			}
		}
	}
	metrics, err := json.Marshal(evaluation.Report{FormatVersion: 1, Trials: trials})
	if err != nil {
		t.Fatal(err)
	}
	assignmentsJSON, err := json.Marshal(assignments)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"metrics.json": metrics, "blind-key.json": assignmentsJSON} {
		if err := os.WriteFile(filepath.Join(runDir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	scorePath := filepath.Join(runDir, "scores.csv")
	if err := os.WriteFile(scorePath, []byte(rows.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"eval-report", "-run", runDir, "-scores", scorePath, "-baseline", "current-auto", "-seed", "123", "-replicates", "500"}, nil, &stdout, &stderr, noEnv)
	if code != 0 {
		t.Fatalf("exit = %d; stderr = %q", code, stderr.String())
	}
	for _, want := range []string{"difficulty=all category=all gate=cost_savings", "difficulty=simple category=all gate=latency_p95_regression", "provider_account_billing=external provider account evidence required", "verdict=PASS", "reported=100.0%", "inter_rater_agreement raters=rater-a,rater-b", "quadratic_weighted_kappa=1.0000"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("report output missing %q: %s", want, stdout.String())
		}
	}
	stdout.Reset()
	stderr.Reset()
	code = cli.Run(t.Context(), []string{"eval-report", "-run", runDir, "-baseline", "current-auto", "-seed", "123", "-replicates", "500"}, nil, &stdout, &stderr, noEnv)
	if code != 0 || !strings.Contains(stdout.String(), "category=all gate=quality_difference estimate=n/a points interval=[n/a, n/a] points verdict=INCONCLUSIVE") || !strings.Contains(stdout.String(), "category=all gate=severe_failure_difference estimate=n/a percentage points interval=[n/a, n/a] percentage points verdict=INCONCLUSIVE") {
		t.Errorf("unrated report exit=%d; stdout=%q; stderr=%q", code, stdout.String(), stderr.String())
	}
}
