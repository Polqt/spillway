package cli

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
)

func evalCommand(ctx context.Context, args []string, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) int {
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
	err = evaluation.Run(ctx, client, set, evaluation.Options{Endpoint: *endpoint, APIKey: key, Arms: arms, Repeats: *repeats, Seed: *seed, Timeout: *timeout, Prices: prices, Output: *output})
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

func evalScoreCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("eval-score", flag.ContinueOnError)
	fs.SetOutput(stderr)
	runDir := fs.String("run", "", "evaluation artifact directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *runDir == "" {
		report(stderr, "spillway: eval-score requires -run\n")
		return 2
	}
	var answers []evaluation.Answer
	if err := readJSONLines(ctx, filepath.Join(*runDir, "answers.jsonl"), &answers); err != nil {
		report(stderr, "spillway: eval-score: read blinded answers: %v\n", err)
		return 1
	}
	var assignments []evaluation.Assignment
	if err := readJSON(ctx, filepath.Join(*runDir, "blind-key.json"), &assignments); err != nil {
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
	// #nosec G304 -- path is within the operator-selected evaluation directory.
	file, err := os.OpenFile(qualityPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	opened := err == nil
	if err == nil {
		encoder := json.NewEncoder(file)
		encoder.SetIndent("", "  ")
		err = encoder.Encode(metrics)
		if ctxErr := ctx.Err(); err == nil && ctxErr != nil {
			err = ctxErr
		}
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		if opened {
			_ = os.Remove(qualityPath)
		}
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

func evalReportCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("eval-report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	runDir := fs.String("run", "", "evaluation artifact directory")
	scorePaths := fs.String("scores", "", "comma-separated completed rater CSV sheets")
	baseline := fs.String("baseline", "", "baseline arm name")
	seed := fs.Int64("seed", 1, "paired bootstrap random seed")
	replicates := fs.Int("replicates", 10000, "paired prompt-cluster bootstrap replicates")
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
	var assignments []evaluation.Assignment
	if err := readJSON(ctx, filepath.Join(*runDir, "blind-key.json"), &assignments); err != nil {
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
	gateReport, err := evaluation.BuildGateReport(run, assignments, sheets, evaluation.GateOptions{Baseline: *baseline, Seed: *seed, Replicates: *replicates})
	if err != nil {
		report(stderr, "spillway: eval-report: %v\n", err)
		return 1
	}
	if err := printGateReport(stdout, gateReport); err != nil {
		report(stderr, "spillway: eval-report: write report: %v\n", err)
		return 1
	}
	return 0
}

func printGateReport(w io.Writer, result evaluation.GateReport) error {
	if _, err := fmt.Fprintf(w, "baseline=%s seed=%d replicates=%d\n", result.Baseline, result.Seed, result.Replicates); err != nil {
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
		if _, err := fmt.Fprintf(w, "arm=%s difficulty=%s gate=%s estimate=%s %s interval=%s %s verdict=%s", gate.Arm, gate.Difficulty, gate.Gate, estimate, unit, interval, unit, gate.Verdict); err != nil {
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
		if _, err := fmt.Fprintf(w, "arm=%s difficulty=%s known_cost=%.1f%%", cost.Arm, cost.Difficulty, cost.KnownCostShare*100); err != nil {
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
	agreement := result.Agreement
	if !agreement.Available {
		_, err := fmt.Fprintln(w, "inter_rater_agreement=INCONCLUSIVE (fewer than two raters with overlapping answers)")
		return err
	}
	_, err := fmt.Fprintf(w, "inter_rater_agreement raters=%s paired_ratings=%d exact=%.4f quadratic_weighted_kappa=%.4f\n", strings.Join(agreement.Raters, ","), agreement.PairedRatings, agreement.ExactAgreement, agreement.QuadraticWeightedKappa)
	return err
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
