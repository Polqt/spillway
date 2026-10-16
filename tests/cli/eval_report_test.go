package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Polqt/spillway/internal/cli"
	"github.com/Polqt/spillway/internal/evaluation"
)

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
			trials = append(trials, evaluation.Trial{AnswerID: answerID, PromptID: promptID, Difficulty: "simple", Arm: arm, Repeat: 1, LatencyMS: 100, CostUSD: cost, CostSource: "reported"})
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
	for _, want := range []string{"difficulty=all gate=cost_savings", "difficulty=simple gate=latency_p95_regression", "verdict=PASS", "reported=100.0%", "inter_rater_agreement raters=rater-a,rater-b", "quadratic_weighted_kappa=1.0000"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("report output missing %q: %s", want, stdout.String())
		}
	}
	stdout.Reset()
	stderr.Reset()
	code = cli.Run(t.Context(), []string{"eval-report", "-run", runDir, "-baseline", "current-auto", "-seed", "123", "-replicates", "500"}, nil, &stdout, &stderr, noEnv)
	if code != 0 || !strings.Contains(stdout.String(), "gate=quality_difference estimate=n/a points interval=[n/a, n/a] points verdict=INCONCLUSIVE") || !strings.Contains(stdout.String(), "gate=severe_failure_difference estimate=n/a percentage points interval=[n/a, n/a] percentage points verdict=INCONCLUSIVE") {
		t.Errorf("unrated report exit=%d; stdout=%q; stderr=%q", code, stdout.String(), stderr.String())
	}
}
