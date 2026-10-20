package evaluation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const maxPilotReportBytes = 64 << 20

func verifyMockEndpoints(ctx context.Context, client *http.Client, endpoints map[string]*url.URL, apiKey string) error {
	for arm, endpoint := range endpoints {
		modelsURL := endpoint.Scheme + "://" + endpoint.Host + "/v1/models"
		requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, modelsURL, nil)
		if err != nil {
			cancel()
			return errors.New("build mock pilot model check")
		}
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			return fmt.Errorf("check mock pilot endpoint for arm %q: %w", arm, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
		closeErr := resp.Body.Close()
		cancel()
		if readErr != nil || closeErr != nil {
			return fmt.Errorf("read mock pilot model list for arm %q", arm)
		}
		if len(data) > 1<<20 || resp.StatusCode != http.StatusOK {
			return fmt.Errorf("mock pilot endpoint for arm %q returned an invalid model list", arm)
		}
		var catalog struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(data, &catalog); err != nil || len(catalog.Data) == 0 {
			return fmt.Errorf("mock pilot endpoint for arm %q returned an invalid model list", arm)
		}
		mockFound := false
		for _, model := range catalog.Data {
			if strings.HasPrefix(model.ID, "mock/") {
				mockFound = true
				continue
			}
			if model.ID != "auto" && !strings.HasPrefix(model.ID, "spillway/") {
				return fmt.Errorf("mock pilot endpoint for arm %q includes a non-mock model", arm)
			}
		}
		if !mockFound {
			return fmt.Errorf("mock pilot endpoint for arm %q has no mock model", arm)
		}
	}
	return nil
}

func verifyMockPilotReport(ctx context.Context, path, candidateSetSHA256 string, arms []Arm) error {
	if path == "" {
		return errors.New("live rules benchmark requires a mock pilot report")
	}
	file, err := os.Open(path) // #nosec G304 -- the path is the operator-provided pilot report.
	if err != nil {
		return fmt.Errorf("open mock pilot report: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxPilotReportBytes+1))
	closeErr := file.Close()
	if err != nil {
		return fmt.Errorf("read mock pilot report: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("close mock pilot report: %w", closeErr)
	}
	if len(data) > maxPilotReportBytes {
		return errors.New("mock pilot report exceeds 64 MiB")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var report Report
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&report); err != nil {
		return errors.New("decode mock pilot report")
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("mock pilot report must contain one JSON value")
	}
	if report.FormatVersion < 3 || report.CandidateSetSHA256 != candidateSetSHA256 || !sameArms(report.CandidateSet, arms) {
		return errors.New("mock pilot report does not match the approved candidate-set attestation and arms")
	}
	expected := report.PromptCount * report.Environment.Repeats * len(arms)
	if report.PromptCount == 0 || report.Environment.Repeats == 0 || len(report.Trials) != expected {
		return errors.New("mock pilot report is incomplete")
	}
	seenTrials := make(map[string]bool, expected)
	prompts := make(map[string]map[string]bool)
	armNames := make(map[string]bool, len(arms))
	for _, arm := range arms {
		armNames[arm.Name] = true
	}
	for _, trial := range report.Trials {
		if trial.Provider != "mock" || trial.Outcome != OutcomeSucceeded || trial.CostUSD != 0 {
			return errors.New("mock pilot report contains a non-mock, failed, or non-zero-cost trial")
		}
		key := trialKey(trial.PromptID, trial.Arm, trial.Repeat)
		if trial.PromptID == "" || trial.AnswerID == "" || !armNames[trial.Arm] || trial.Repeat < 1 || trial.Repeat > report.Environment.Repeats || seenTrials[key] {
			return errors.New("mock pilot report contains an incomplete or duplicate trial")
		}
		seenTrials[key] = true
		if prompts[trial.PromptID] == nil {
			prompts[trial.PromptID] = make(map[string]bool)
		}
		prompts[trial.PromptID][trial.Difficulty] = true
	}
	if len(prompts) != report.PromptCount {
		return errors.New("mock pilot report prompt count does not match its trials")
	}
	difficultyCounts := make(map[string]int)
	for _, difficulties := range prompts {
		if len(difficulties) != 1 {
			return errors.New("mock pilot report has conflicting prompt difficulty labels")
		}
		for difficulty := range difficulties {
			difficultyCounts[difficulty]++
		}
	}
	if difficultyCounts["simple"] == 0 || difficultyCounts["moderate"] == 0 || difficultyCounts["hard"] == 0 {
		return errors.New("mock pilot report must cover every difficulty")
	}
	return nil
}

func sameArms(first, second []Arm) bool {
	if len(first) != len(second) {
		return false
	}
	byName := make(map[string]string, len(first))
	for _, arm := range first {
		byName[arm.Name] = arm.Model
	}
	for _, arm := range second {
		if byName[arm.Name] != arm.Model {
			return false
		}
	}
	return true
}
