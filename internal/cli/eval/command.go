package eval

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Polqt/spillway/internal/evaluation"
	"github.com/Polqt/spillway/internal/store"
)

// Run executes a paired routing evaluation.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) int {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	datasetPath := fs.String("dataset", "", "versioned prompt set JSON path")
	endpoint := fs.String("endpoint", "http://127.0.0.1:8080", "Spillway OpenAI-compatible base URL")
	keyEnv := fs.String("api-key-env", "", "environment variable holding the local API key")
	armList := fs.String("arms", "", "comma-separated name=model[@endpoint] arms, for example current=auto")
	repeats := fs.Int("repeats", 1, "repetitions per prompt and arm (1-100)")
	seed := fs.Int64("seed", 0, "randomization seed; zero chooses a random seed")
	timeout := fs.Duration("timeout", 2*time.Hour, "whole-run deadline")
	pricePath := fs.String("prices", "", "optional JSON map of model references to declared prices")
	output := fs.String("out", "", "directory for evaluation artifacts")
	reviewExport := fs.String("review-export-dir", "", "explicit temporary directory for blinded prompt and response review material")
	resume := fs.Bool("resume", false, "resume an existing run after verifying its corpus and settings fingerprint")
	retryFailed := fs.String("retry-failed", "", "comma-separated exact failed trial IDs to retry; duplicate provider charges may occur")
	rulesBenchmark := fs.Bool("rules-benchmark", false, "validate sample size, repeats, seed and arms for a rules benchmark")
	candidateSetSHA256 := fs.String("candidate-set-sha256", "", "operator-attested shared candidate-set SHA-256 digest")
	benchmarkPilot := fs.Bool("benchmark-pilot", false, "run a zero-spend mock rules benchmark pilot")
	benchmarkLive := fs.Bool("benchmark-live", false, "run a paid rules benchmark after required approvals")
	approveLiveRun := fs.Bool("approve-live-run", false, "record separate owner approval for this live benchmark run")
	approveCandidateSet := fs.Bool("approve-candidate-set", false, "record separate owner approval for the candidate set")
	approveT19Recovery := fs.Bool("approve-t19-recovery", false, "record resolution of T19 or approval of its documented recovery path")
	mockPilotReport := fs.String("mock-pilot-report", "", "completed zero-spend mock pilot report required for a live benchmark")
	mockPilotInspected := fs.Bool("mock-pilot-inspected", false, "confirm the zero-spend mock pilot was inspected")
	costCeilingUSD := fs.Float64("cost-ceiling-usd", 0, "separately approved estimated-cost ceiling for the live run in USD")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *datasetPath == "" || *armList == "" || *output == "" {
		report(stderr, "spillway: eval requires -dataset, -arms and -out\n")
		return 2
	}
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	if err := ctx.Err(); err != nil {
		report(stderr, "spillway: eval: %v\n", err)
		return 1
	}
	file, err := os.Open(*datasetPath)
	if err != nil {
		report(stderr, "spillway: eval: open prompt set: %v\n", err)
		return 1
	}
	set, err := evaluation.LoadPromptSet(contextReader{ctx: ctx, reader: file})
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if ctxErr := ctx.Err(); err == nil && ctxErr != nil {
		err = ctxErr
	}
	if err != nil {
		report(stderr, "spillway: eval: %v\n", err)
		return 1
	}
	arms, err := parseArms(*armList)
	if err != nil {
		report(stderr, "spillway: eval: %v\n", err)
		return 2
	}
	var prices map[string]evaluation.Price
	if *pricePath != "" {
		prices, err = loadEvaluationPrices(ctx, *pricePath)
		if err != nil {
			report(stderr, "spillway: eval: %v\n", err)
			return 1
		}
	}
	key := ""
	if *keyEnv != "" {
		var ok bool
		key, ok = lookupEnv(*keyEnv)
		if !ok || key == "" {
			report(stderr, "spillway: eval: environment variable %s is not set\n", *keyEnv)
			return 1
		}
	}
	client := &http.Client{}
	if *reviewExport != "" && pathsOverlap(*output, *reviewExport) {
		report(stderr, "spillway: eval: review export directory must be separate from the durable output directory\n")
		return 2
	}
	var retryIDs []string
	if *retryFailed != "" {
		retryIDs = strings.Split(*retryFailed, ",")
		for i := range retryIDs {
			retryIDs[i] = strings.TrimSpace(retryIDs[i])
		}
	}
	err = evaluation.Run(ctx, client, set, evaluation.Options{
		Endpoint: *endpoint, APIKey: key, Arms: arms, Repeats: *repeats, Seed: *seed, Timeout: *timeout,
		Prices: prices, Output: *output, ReviewExportDir: *reviewExport, Resume: *resume, RetryFailedIDs: retryIDs,
		RulesBenchmark: *rulesBenchmark, CandidateSetSHA256: *candidateSetSHA256, BenchmarkPilot: *benchmarkPilot,
		BenchmarkLive: *benchmarkLive, LiveRunApproved: *approveLiveRun, CandidateSetApproved: *approveCandidateSet,
		T19RecoveryApproved: *approveT19Recovery, MockPilotReport: *mockPilotReport,
		MockPilotInspected: *mockPilotInspected, CostCeilingUSD: *costCeilingUSD,
	})
	if err != nil {
		report(stderr, "spillway: eval: %v\n", err)
		return 1
	}
	_, err = fmt.Fprintf(stdout, "evaluation artifacts written to %s\n", *output)
	if err != nil {
		report(stderr, "spillway: eval: write result path: %v\n", err)
		return 1
	}
	return 0
}

// RunScore summarizes a completed blinded evaluation score sheet.
func RunScore(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("eval-score", flag.ContinueOnError)
	fs.SetOutput(stderr)
	runDir := fs.String("run", "", "evaluation artifact directory")
	reviewDir := fs.String("review-dir", "", "temporary review export directory containing the blind key")
	reviewer := fs.Int("reviewer", 1, "review export view to summarize (1 or 2)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *runDir == "" {
		report(stderr, "spillway: eval-score requires -run\n")
		return 2
	}
	if *reviewDir == "" {
		*reviewDir = *runDir
	}
	if *reviewer < 1 || *reviewer > 2 {
		report(stderr, "spillway: eval-score: reviewer must be 1 or 2\n")
		return 2
	}
	var answers []evaluation.Answer
	if err := readJSONLines(ctx, filepath.Join(*reviewDir, fmt.Sprintf("rater-%d.jsonl", *reviewer)), &answers); err != nil {
		report(stderr, "spillway: eval-score: read blinded answers: %v\n", err)
		return 1
	}
	var assignments []evaluation.Assignment
	if err := readJSON(ctx, filepath.Join(*reviewDir, "blind-key.json"), &assignments); err != nil {
		report(stderr, "spillway: eval-score: read assignment key: %v\n", err)
		return 1
	}
	if err := ctx.Err(); err != nil {
		report(stderr, "spillway: eval-score: %v\n", err)
		return 1
	}
	scoreFile, err := os.Open(filepath.Join(*runDir, "scores.csv"))
	if err != nil {
		report(stderr, "spillway: eval-score: open score sheet: %v\n", err)
		return 1
	}
	scores, err := evaluation.LoadScores(contextReader{ctx: ctx, reader: scoreFile})
	if closeErr := scoreFile.Close(); err == nil {
		err = closeErr
	}
	if ctxErr := ctx.Err(); err == nil && ctxErr != nil {
		err = ctxErr
	}
	if err != nil {
		report(stderr, "spillway: eval-score: %v\n", err)
		return 1
	}
	if err := ctx.Err(); err != nil {
		report(stderr, "spillway: eval-score: %v\n", err)
		return 1
	}
	metrics, err := evaluation.SummarizeScores(answers, assignments, scores)
	if err != nil {
		report(stderr, "spillway: eval-score: %v\n", err)
		return 1
	}
	if err := ctx.Err(); err != nil {
		report(stderr, "spillway: eval-score: %v\n", err)
		return 1
	}
	qualityPath := filepath.Join(*runDir, "quality.json")
	err = writeQualitySummary(ctx, *runDir, qualityPath, metrics)
	if err != nil {
		report(stderr, "spillway: eval-score: write quality summary: %v\n", err)
		return 1
	}
	_, err = fmt.Fprintf(stdout, "quality summary written to %s\n", filepath.Join(*runDir, "quality.json"))
	if err != nil {
		report(stderr, "spillway: eval-score: write result path: %v\n", err)
		return 1
	}
	return 0
}

func pathsOverlap(first, second string) bool {
	firstAbs, firstErr := filepath.Abs(first)
	secondAbs, secondErr := filepath.Abs(second)
	if firstErr != nil || secondErr != nil {
		return true
	}
	return pathContains(firstAbs, secondAbs) || pathContains(secondAbs, firstAbs)
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	if err != nil {
		return true
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func writeQualitySummary(ctx context.Context, directory, path string, metrics []evaluation.QualityMetrics) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// #nosec G304 -- the temporary file is created inside the operator-selected evaluation directory.
	file, err := os.CreateTemp(directory, ".quality-*.tmp")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	removeTemp := func(writeErr error) error {
		if removeErr := os.Remove(tempPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return errors.Join(writeErr, fmt.Errorf("remove temporary quality summary: %w", removeErr))
		}
		return writeErr
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	writeErr := encoder.Encode(metrics)
	if ctxErr := ctx.Err(); writeErr == nil && ctxErr != nil {
		writeErr = ctxErr
	}
	writeErr = errors.Join(writeErr, file.Close())
	if writeErr != nil {
		return removeTemp(writeErr)
	}
	if err := ctx.Err(); err != nil {
		return removeTemp(err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return removeTemp(err)
	}
	return nil
}

// RunReport compares evaluation artifacts and score sheets against the routing gates.
func RunReport(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("eval-report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	runDir := fs.String("run", "", "evaluation artifact directory")
	reviewDir := fs.String("review-dir", "", "temporary review export directory containing the blind key")
	scorePaths := fs.String("scores", "", "comma-separated completed rater CSV sheets")
	baseline := fs.String("baseline", "", "baseline arm name")
	seed := fs.Int64("seed", 1, "paired bootstrap random seed")
	replicates := fs.Int("replicates", 10000, "paired prompt-cluster bootstrap replicates")
	jsonOutput := fs.Bool("json", false, "write the safe aggregate report as JSON")
	storePath := fs.String("store", "", "optional SQLite store path for retaining the aggregate report")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *runDir == "" || *baseline == "" {
		report(stderr, "spillway: eval-report requires -run and -baseline\n")
		return 2
	}
	var run evaluation.Report
	if err := readJSON(ctx, filepath.Join(*runDir, "metrics.json"), &run); err != nil {
		report(stderr, "spillway: eval-report: read metrics: %v\n", err)
		return 1
	}
	if *reviewDir == "" {
		*reviewDir = *runDir
	}
	var assignments []evaluation.Assignment
	if err := readJSON(ctx, filepath.Join(*reviewDir, "blind-key.json"), &assignments); err != nil {
		report(stderr, "spillway: eval-report: read assignment key: %v\n", err)
		return 1
	}
	var paths []string
	if *scorePaths != "" {
		paths = strings.Split(*scorePaths, ",")
	}
	sheets := make([][]evaluation.Score, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			report(stderr, "spillway: eval-report: score paths cannot be empty\n")
			return 2
		}
		if err := ctx.Err(); err != nil {
			report(stderr, "spillway: eval-report: %v\n", err)
			return 1
		}
		// #nosec G304 -- path is an operator-selected local score sheet.
		file, err := os.Open(path)
		if err != nil {
			report(stderr, "spillway: eval-report: open score sheet: %v\n", err)
			return 1
		}
		scores, loadErr := evaluation.LoadScores(contextReader{ctx: ctx, reader: file})
		if closeErr := file.Close(); loadErr == nil {
			loadErr = closeErr
		}
		if ctxErr := ctx.Err(); loadErr == nil && ctxErr != nil {
			loadErr = ctxErr
		}
		if loadErr != nil {
			report(stderr, "spillway: eval-report: load score sheet: %v\n", loadErr)
			return 1
		}
		sheets = append(sheets, scores)
	}
	gateReport, err := evaluation.BuildGateReport(ctx, run, assignments, sheets, evaluation.GateOptions{Baseline: *baseline, Seed: *seed, Replicates: *replicates})
	if err != nil {
		report(stderr, "spillway: eval-report: %v\n", err)
		return 1
	}
	if *storePath != "" {
		summary, marshalErr := json.Marshal(gateReport)
		if marshalErr != nil {
			report(stderr, "spillway: eval-report: encode aggregate report: %v\n", marshalErr)
			return 1
		}
		database, openErr := store.Open(ctx, store.Options{Path: *storePath, Retention: 90 * 24 * time.Hour})
		if openErr != nil {
			report(stderr, "spillway: eval-report: open aggregate store: %v\n", openErr)
			return 1
		}
		record := store.EvaluationRun{RunID: fmt.Sprintf("eval-%d", time.Now().UTC().UnixNano()), CreatedAt: time.Now().UTC(), CorpusSHA256: gateReport.CorpusSHA256, Summary: summary}
		storeErr := database.RecordEvaluation(ctx, record)
		closeErr := database.Close()
		if storeErr != nil || closeErr != nil {
			report(stderr, "spillway: eval-report: save aggregate report: %v\n", errors.Join(storeErr, closeErr))
			return 1
		}
	}
	var printErr error
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		printErr = encoder.Encode(gateReport)
	} else {
		printErr = printGateReport(stdout, gateReport)
	}
	if printErr != nil {
		report(stderr, "spillway: eval-report: write report: %v\n", printErr)
		return 1
	}
	return 0
}

func printGateReport(w io.Writer, result evaluation.GateReport) error {
	if _, err := fmt.Fprintf(w, "run format_version=%d prompt_set_version=%d prompt_count=%d corpus_sha256=%s settings_sha256=%s\n", result.FormatVersion, result.PromptSetVersion, result.PromptCount, result.CorpusSHA256, result.SettingsSHA256); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "baseline=%s seed=%d replicates=%d\n", result.Baseline, result.Seed, result.Replicates); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "provider_account_billing=%s\n", result.BillingStatus); err != nil {
		return err
	}
	for _, gate := range result.Results {
		unit := gate.Unit
		if gate.Gate == "quality_difference" {
			unit = "points"
		}
		estimate, interval := "n/a", "[n/a, n/a]"
		if gate.Available {
			estimate = fmt.Sprintf("%.4f", gate.Estimate)
			interval = fmt.Sprintf("[%.4f, %.4f]", gate.Lower, gate.Upper)
		}
		if _, err := fmt.Fprintf(w, "arm=%s difficulty=%s category=%s gate=%s estimate=%s %s interval=%s %s verdict=%s", gate.Arm, gate.Difficulty, gate.Category, gate.Gate, estimate, unit, interval, unit, gate.Verdict); err != nil {
			return err
		}
		if gate.Reason != "" {
			if _, err := fmt.Fprintf(w, " reason=%s", gate.Reason); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	for _, cost := range result.Cost {
		if _, err := fmt.Fprintf(w, "arm=%s difficulty=%s category=%s known_cost=%.1f%%", cost.Arm, cost.Difficulty, cost.Category, cost.KnownCostShare*100); err != nil {
			return err
		}
		keys := make([]string, 0, len(cost.CostSourceShares))
		for key := range cost.CostSourceShares {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if _, err := fmt.Fprintf(w, " %s=%.1f%%", key, cost.CostSourceShares[key]*100); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	for _, metrics := range result.RunMetrics {
		if _, err := fmt.Fprintf(w, "arm=%s difficulty=%s category=%s requests=%d succeeded=%d failed=%d timed_out=%d cancelled=%d incomplete_streams=%d exhausted=%d zero_cost=%d status_classes=%s spillover_selected=%d spillover_succeeded=%d latency_p50_ms=%d latency_p95_ms=%d first_token_p50_ms=%d first_token_p95_ms=%d\n", metrics.Arm, metrics.Difficulty, metrics.Category, metrics.Requests, metrics.Successful, metrics.Failed, metrics.TimedOut, metrics.Cancelled, metrics.IncompleteStreams, metrics.Exhausted, metrics.ZeroCostRequests, formatStatusClasses(metrics.StatusClassShare), metrics.SpilloverPicked, metrics.SpilloverOK, metrics.LatencyP50MS, metrics.LatencyP95MS, metrics.FirstTokenP50MS, metrics.FirstTokenP95MS); err != nil {
			return err
		}
	}
	agreement := result.Agreement
	if !agreement.Available {
		_, err := fmt.Fprintln(w, "inter_rater_agreement=INCONCLUSIVE (fewer than two raters with overlapping answers)")
		return err
	}
	_, err := fmt.Fprintf(w, "inter_rater_agreement raters=%s paired_ratings=%d exact=%.4f quadratic_weighted_kappa=%.4f\n", strings.Join(agreement.Raters, ","), agreement.PairedRatings, agreement.ExactAgreement, agreement.QuadraticWeightedKappa)
	return err
}

func formatStatusClasses(shares map[string]float64) string {
	keys := make([]string, 0, len(shares))
	for key := range shares {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		values = append(values, fmt.Sprintf("%s=%.4f", key, shares[key]))
	}
	return strings.Join(values, ",")
}

func readJSON(ctx context.Context, path string, value any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// #nosec G304 -- path is within the operator-selected evaluation directory.
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(contextReader{ctx: ctx, reader: file})
	decoder.DisallowUnknownFields()
	err = decoder.Decode(value)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return ctx.Err()
}

func readJSONLines(ctx context.Context, path string, values *[]evaluation.Answer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// #nosec G304 -- path is within the operator-selected evaluation directory.
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(contextReader{ctx: ctx, reader: file})
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var answer evaluation.Answer
		if err := json.Unmarshal(scanner.Bytes(), &answer); err != nil {
			return errors.Join(err, file.Close())
		}
		*values = append(*values, answer)
	}
	if err := scanner.Err(); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(err, file.Close())
	}
	return file.Close()
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func parseArms(value string) ([]evaluation.Arm, error) {
	parts := strings.Split(value, ",")
	arms := make([]evaluation.Arm, 0, len(parts))
	for _, part := range parts {
		name, model, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || name == "" || model == "" {
			return nil, errors.New("each arm must be name=model")
		}
		model, endpoint, hasEndpoint := strings.Cut(model, "@")
		arm := evaluation.Arm{Name: name, Model: model}
		if hasEndpoint {
			arm.Endpoint = endpoint
		}
		arms = append(arms, arm)
	}
	return arms, nil
}

func loadEvaluationPrices(ctx context.Context, path string) (map[string]evaluation.Price, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// #nosec G304 -- path is the operator-selected local prices file.
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read prices: %w", err)
	}
	var prices map[string]evaluation.Price
	decoder := json.NewDecoder(contextReader{ctx: ctx, reader: file})
	decoder.DisallowUnknownFields()
	err = decoder.Decode(&prices)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if ctxErr := ctx.Err(); err == nil && ctxErr != nil {
		err = ctxErr
	}
	if err != nil {
		return nil, fmt.Errorf("decode prices: %w", err)
	}
	if prices == nil {
		return nil, errors.New("prices must be a JSON object")
	}
	return prices, nil
}

func report(stderr io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(stderr, format, args...)
}
