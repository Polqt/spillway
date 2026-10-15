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
	file, err := os.Open(*datasetPath)
	if err != nil {
		report(stderr, "spillway: eval: open prompt set: %v\n", err)
		return 1
	}
	set, err := evaluation.LoadPromptSet(file)
	if closeErr := file.Close(); err == nil {
		err = closeErr
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
		prices, err = loadEvaluationPrices(*pricePath)
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

func evalScoreCommand(args []string, stdout, stderr io.Writer) int {
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
	if err := readJSONLines(filepath.Join(*runDir, "answers.jsonl"), &answers); err != nil {
		report(stderr, "spillway: eval-score: read blinded answers: %v\n", err)
		return 1
	}
	var assignments []evaluation.Assignment
	if err := readJSON(filepath.Join(*runDir, "blind-key.json"), &assignments); err != nil {
		report(stderr, "spillway: eval-score: read assignment key: %v\n", err)
		return 1
	}
	scoreFile, err := os.Open(filepath.Join(*runDir, "scores.csv"))
	if err != nil {
		report(stderr, "spillway: eval-score: open score sheet: %v\n", err)
		return 1
	}
	scores, err := evaluation.LoadScores(scoreFile)
	if closeErr := scoreFile.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		report(stderr, "spillway: eval-score: %v\n", err)
		return 1
	}
	metrics, err := evaluation.SummarizeScores(answers, assignments, scores)
	if err != nil {
		report(stderr, "spillway: eval-score: %v\n", err)
		return 1
	}
	file, err := os.OpenFile(filepath.Join(*runDir, "quality.json"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err == nil {
		encoder := json.NewEncoder(file)
		encoder.SetIndent("", "  ")
		err = encoder.Encode(metrics)
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}
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

func readJSON(path string, value any) error {
	// #nosec G304 -- path is within the operator-selected evaluation directory.
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	return nil
}

func readJSONLines(path string, values *[]evaluation.Answer) error {
	// #nosec G304 -- path is within the operator-selected evaluation directory.
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(file)
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
	return file.Close()
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

func loadEvaluationPrices(path string) (map[string]evaluation.Price, error) {
	// #nosec G304 -- path is the operator-selected local prices file.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read prices: %w", err)
	}
	var prices map[string]evaluation.Price
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&prices); err != nil {
		return nil, fmt.Errorf("decode prices: %w", err)
	}
	if prices == nil {
		return nil, errors.New("prices must be a JSON object")
	}
	return prices, nil
}
