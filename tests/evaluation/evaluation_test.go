package evaluation_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Polqt/spillway/internal/evaluation"
)

func TestRun_ResumeSkipsCompletedTrialsAndPersistsPartialRun(t *testing.T) {
	set, err := evaluation.LoadPromptSet(strings.NewReader(`{"version":1,"prompts":[{"id":"p1","category":"coding","difficulty":"simple","prompt":"test one"},{"id":"p2","category":"reasoning","difficulty":"moderate","prompt":"test two"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"}}]}`)
		_, _ = fmt.Fprintln(w, "data: [DONE]")
		cancel()
	}))
	defer server.Close()
	out := filepath.Join(t.TempDir(), "run")
	review := filepath.Join(t.TempDir(), "review")
	options := evaluation.Options{Endpoint: server.URL, Arms: []evaluation.Arm{{Name: "auto", Model: "auto"}, {Name: "rules", Model: "spillway/simple"}}, Repeats: 1, Seed: 23, Output: out, ReviewExportDir: review}
	if err := evaluation.Run(ctx, server.Client(), set, options); err == nil {
		t.Fatal("first run error = nil, want cancellation after its first completed trial")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("first run made %d requests, want 1", got)
	}
	options.Resume = true
	if err := evaluation.Run(t.Context(), server.Client(), set, options); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("total requests after resume = %d, want 4 (completed trial skipped)", got)
	}
	// #nosec G304
	data, err := os.ReadFile(filepath.Join(out, "metrics.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report evaluation.Report
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Trials) != 4 || report.Seed != 23 {
		t.Fatalf("resumed report has %d trials and seed %d, want 4 and 23", len(report.Trials), report.Seed)
	}
	var successful int
	for _, trial := range report.Trials {
		if trial.Outcome == evaluation.OutcomeSucceeded {
			successful++
		}
	}
	// #nosec G304 -- review is a test-owned temporary directory.
	view, err := os.ReadFile(filepath.Join(review, "rater-1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(view), `"trial_id"`); got != successful {
		t.Fatalf("review export has %d answers, want every successful trial (%d)", got, successful)
	}
	// #nosec G304 -- review is a test-owned temporary directory.
	checklist, err := os.ReadFile(filepath.Join(review, "DELETE_AFTER_RATING.txt"))
	if err != nil || !strings.Contains(string(checklist), ".answers-work.jsonl") {
		t.Fatalf("deletion checklist = %q, err %v", checklist, err)
	}
}

func TestRun_RetriesOnlyNamedFailedTrial(t *testing.T) {
	set, err := evaluation.LoadPromptSet(strings.NewReader(`{"version":1,"prompts":[{"id":"p1","category":"coding","difficulty":"simple","prompt":"retry test"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var retryCalls atomic.Int32
	var totalCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		totalCalls.Add(1)
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if request.Model == "retry" && retryCalls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"}}]}`)
		_, _ = fmt.Fprintln(w, "data: [DONE]")
	}))
	defer server.Close()
	out := filepath.Join(t.TempDir(), "run")
	options := evaluation.Options{Endpoint: server.URL, Arms: []evaluation.Arm{{Name: "auto", Model: "auto"}, {Name: "retry-arm", Model: "retry"}}, Repeats: 1, Seed: 4, Output: out}
	if err := evaluation.Run(t.Context(), server.Client(), set, options); err != nil {
		t.Fatal(err)
	}
	// #nosec G304 -- out is a test-owned temporary directory.
	data, err := os.ReadFile(filepath.Join(out, "metrics.json"))
	if err != nil {
		t.Fatal(err)
	}
	var partial evaluation.Report
	if err := json.Unmarshal(data, &partial); err != nil {
		t.Fatal(err)
	}
	var failedID string
	for _, trial := range partial.Trials {
		if trial.Outcome != evaluation.OutcomeSucceeded {
			failedID = trial.AnswerID
		}
	}
	if failedID == "" {
		t.Fatal("initial report has no failed trial to retry")
	}
	options.Resume = true
	options.RetryFailedIDs = []string{failedID}
	if err := evaluation.Run(t.Context(), server.Client(), set, options); err != nil {
		t.Fatal(err)
	}
	if got := totalCalls.Load(); got != 3 {
		t.Fatalf("total requests = %d, want 3 (one named retry)", got)
	}
	// #nosec G304 -- out is a test-owned temporary directory.
	data, err = os.ReadFile(filepath.Join(out, "metrics.json"))
	if err != nil {
		t.Fatal(err)
	}
	var resumed evaluation.Report
	if err := json.Unmarshal(data, &resumed); err != nil {
		t.Fatal(err)
	}
	for _, trial := range resumed.Trials {
		if trial.AnswerID == failedID && trial.Outcome != evaluation.OutcomeSucceeded {
			t.Fatalf("retried trial outcome = %q, want succeeded", trial.Outcome)
		}
	}
}

func TestRun_ClassifiesIncompleteStreamAndRecordsFallbackRoute(t *testing.T) {
	set, err := evaluation.LoadPromptSet(strings.NewReader(`{"version":1,"prompts":[{"id":"p1","category":"coding","difficulty":"simple","prompt":"stream test"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("x-spillway-route", "auto; candidate 2/2")
		_, _ = fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"partial"}}]}`)
	}))
	defer server.Close()
	out := filepath.Join(t.TempDir(), "run")
	if err := evaluation.Run(t.Context(), server.Client(), set, evaluation.Options{Endpoint: server.URL, Arms: []evaluation.Arm{{Name: "auto", Model: "auto"}}, Repeats: 1, Seed: 1, Output: out}); err != nil {
		t.Fatal(err)
	}
	// #nosec G304 -- out is a test-owned temporary directory.
	data, err := os.ReadFile(filepath.Join(out, "metrics.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report evaluation.Report
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	trial := report.Trials[0]
	if trial.Outcome != evaluation.OutcomeIncompleteStream || trial.HTTPStatusClass != "2xx" || !trial.SpilloverSelected || trial.LatencyMS < 0 || report.Metrics[0].SpilloverPicked != 1 || report.Metrics[0].SpilloverOK != 0 {
		t.Fatalf("incomplete stream metadata = %#v, metrics = %#v", trial, report.Metrics[0])
	}
}

func TestRun_DoesNotPersistUntrustedRouteHeadersOrProviderErrors(t *testing.T) {
	set, err := evaluation.LoadPromptSet(strings.NewReader(`{"version":1,"prompts":[{"id":"p1","category":"coding","difficulty":"simple","prompt":"header test"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-spillway-route", "Bearer sk-test-route-secret")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "provider error body must not persist")
	}))
	defer server.Close()
	out := filepath.Join(t.TempDir(), "run")
	if err := evaluation.Run(t.Context(), server.Client(), set, evaluation.Options{Endpoint: server.URL, Arms: []evaluation.Arm{{Name: "auto", Model: "auto"}}, Repeats: 1, Seed: 1, Output: out}); err != nil {
		t.Fatal(err)
	}
	// #nosec G304 -- out is a test-owned temporary directory.
	data, err := os.ReadFile(filepath.Join(out, "metrics.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"sk-test-route-secret", "provider error body must not persist", `"route_reason"`} {
		if strings.Contains(string(data), forbidden) {
			t.Errorf("durable evaluation metadata contains %q: %s", forbidden, data)
		}
	}
}

func TestLoadPromptSet_RequiresVersionAndLabels(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data string
		want string
	}{
		{
			name: "valid v1 set",
			data: `{"version":1,"prompts":[{"id":"p1","category":"coding","difficulty":"moderate","prompt":"Write a greeting."}]}`,
		},
		{name: "unsupported version", data: `{"version":3,"prompts":[]}`, want: "version"},
		{name: "missing category", data: `{"version":1,"prompts":[{"id":"p1","difficulty":"moderate","prompt":"hi"}]}`, want: "category"},
		{name: "unknown difficulty", data: `{"version":1,"prompts":[{"id":"p1","category":"coding","difficulty":"easy","prompt":"hi"}]}`, want: "difficulty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			set, err := evaluation.LoadPromptSet(strings.NewReader(tt.data))
			if tt.want != "" {
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if set.Version != 1 || len(set.Prompts) != 1 || set.Prompts[0].Category != "coding" {
				t.Fatalf("set = %#v, want one v1 coding prompt", set)
			}
		})
	}
}

func TestRun_PairsRandomizedArmsAndWritesBlindedArtifacts(t *testing.T) {
	set, err := evaluation.LoadPromptSet(strings.NewReader(`{"version":1,"prompts":[{"id":"p1","category":"coding","difficulty":"moderate","prompt":"private prompt one","settings":{"temperature":0}},{"id":"p2","category":"reasoning","difficulty":"hard","prompt":"private prompt two","settings":{"temperature":0}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var calls []struct{ model, prompt string }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
			StreamOptions struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if !request.StreamOptions.IncludeUsage {
			t.Error("stream request did not ask for usage")
		}
		calls = append(calls, struct{ model, prompt string }{request.Model, request.Messages[0].Content})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"answer-%s\"}}]}\n\n", request.Model)
		_, _ = fmt.Fprintln(w, `data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"cost":0.0001}}`)
		_, _ = fmt.Fprintln(w, "data: [DONE]")
	}))
	defer server.Close()
	out := filepath.Join(t.TempDir(), "run")
	review := filepath.Join(t.TempDir(), "temporary-review")
	err = evaluation.Run(t.Context(), server.Client(), set, evaluation.Options{
		Endpoint:        server.URL,
		Arms:            []evaluation.Arm{{Name: "current", Model: "auto"}, {Name: "candidate", Model: "spillway/simple"}},
		Repeats:         2,
		Seed:            17,
		Output:          out,
		ReviewExportDir: review,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 8 {
		t.Fatalf("request count = %d, want 8", len(calls))
	}
	promptOrders := make(map[string][]string)
	for _, id := range []string{"private prompt one", "private prompt two"} {
		seen := 0
		for _, call := range calls {
			if call.prompt == id {
				seen++
				promptOrders[id] = append(promptOrders[id], call.model)
			}
		}
		if seen != 4 {
			t.Errorf("prompt %q sent %d times, want each arm paired for two repeats", id, seen)
		}
	}
	randomized := false
	for _, order := range promptOrders {
		if order[0] != order[2] {
			randomized = true
		}
	}
	if !randomized {
		t.Errorf("arm order did not vary across prompt repetitions: %#v", promptOrders)
	}
	// #nosec G304 -- out is a test-owned temporary directory.
	metrics, err := os.ReadFile(filepath.Join(out, "metrics.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report evaluation.Report
	if err := json.Unmarshal(metrics, &report); err != nil {
		t.Fatalf("decode metrics artifact: %v", err)
	}
	if strings.Contains(string(metrics), "private prompt") || strings.Contains(string(metrics), "answer-") || len(report.Trials) != 8 || report.Trials[0].CostSource != evaluation.CostReported || report.Trials[0].Outcome != evaluation.OutcomeSucceeded {
		t.Errorf("metrics artifact leaks content or has incorrect trial data: %#v", report)
	}
	if report.FormatVersion != 3 || report.PromptCount != 2 || report.Environment.PromptSetSHA256 == "" || report.SettingsSHA256 == "" || len(report.Categories) != 2 || len(report.CandidateSet) != 2 {
		t.Errorf("metadata contract is incomplete: %#v", report)
	}
	for _, trial := range report.Trials {
		if !strings.HasPrefix(trial.AnswerID, "trial-") || len(trial.AnswerID) != len("trial-")+32 {
			t.Errorf("trial ID is not opaque: %q", trial.AnswerID)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "answers.jsonl")); !os.IsNotExist(err) {
		t.Errorf("durable run unexpectedly contains answers.jsonl: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "blind-key.json")); !os.IsNotExist(err) {
		t.Errorf("durable run unexpectedly contains blind-key.json: %v", err)
	}
	// #nosec G304 -- review is a test-owned temporary directory.
	answers, err := os.ReadFile(filepath.Join(review, "rater-1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(answers), "private prompt") != 4 || strings.Contains(string(answers), `"arm"`) || !strings.Contains(string(answers), "answer-auto") {
		t.Errorf("explicit review export should contain one preselected answer per prompt and arm without arm names: %s", answers)
	}
	// #nosec G304 -- review is a test-owned temporary directory.
	secondView, err := os.ReadFile(filepath.Join(review, "rater-2.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(answers) == string(secondView) {
		t.Errorf("reviewer views use the same serialized order: %s", answers)
	}
	// #nosec G304 -- review is a test-owned temporary directory.
	key, err := os.ReadFile(filepath.Join(review, "blind-key.json"))
	if err != nil {
		t.Fatal(err)
	}
	var assignments []evaluation.Assignment
	if err := json.Unmarshal(key, &assignments); err != nil {
		t.Fatalf("decode blind key: %v", err)
	}
	selectedAssignments := 0
	for _, assignment := range assignments {
		if assignment.RatedForReview {
			selectedAssignments++
		}
	}
	if len(assignments) != 8 || selectedAssignments != 4 || assignments[0].Arm == "" || !strings.Contains(string(key), `"trial_id"`) {
		t.Errorf("blind key has invalid assignments: %#v", assignments)
	}
	if _, err := os.Stat(filepath.Join(review, "DELETE_AFTER_RATING.txt")); err != nil {
		t.Errorf("review export has no deletion checklist: %v", err)
	}
}

func TestSummarizeScores_UsesAssignmentKeyAndReportsSevereFailures(t *testing.T) {
	answers := []evaluation.Answer{
		{AnswerID: "a1", PromptID: "p1", Difficulty: "simple", Category: "coding"},
		{AnswerID: "a2", PromptID: "p1", Difficulty: "simple", Category: "coding"},
	}
	assignments := []evaluation.Assignment{{AnswerID: "a1", Arm: "current"}, {AnswerID: "a2", Arm: "candidate"}}
	scores, err := evaluation.LoadScores(strings.NewReader("answer_id,correctness_1_to_5,completeness_1_to_5,instruction_following_1_to_5,severe_correctness_failure,adjudication_note\na1,4,3,5,false,\na2,1,1,2,true,\n"))
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := evaluation.SummarizeScores(answers, assignments, scores)
	if err != nil {
		t.Fatal(err)
	}
	for _, metric := range metrics {
		if metric.Difficulty != "all" || metric.Category != "all" {
			continue
		}
		if metric.Arm == "current" && (metric.MeanScore != 4 || metric.SevereFailureRate != 0) {
			t.Errorf("current metrics = %#v, want mean 4 and no severe failures", metric)
		}
		if metric.Arm == "candidate" && (metric.MeanScore != 4.0/3.0 || metric.SevereFailureRate != 1) {
			t.Errorf("candidate metrics = %#v, want mean 4/3 and one severe failure", metric)
		}
	}
}

func TestRun_UsesConfiguredEndpointForEachArm(t *testing.T) {
	set, err := evaluation.LoadPromptSet(strings.NewReader(`{"version":1,"prompts":[{"id":"p1","category":"coding","difficulty":"simple","prompt":"test"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var firstCalls, secondCalls atomic.Int32
	var cacheBypassMisses atomic.Int32
	newServer := func(counter *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counter.Add(1)
			if r.Header.Get("X-Spillway-Cache") != "bypass" {
				cacheBypassMisses.Add(1)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"}}]}`)
			_, _ = fmt.Fprintln(w, "data: [DONE]")
		}))
	}
	first, second := newServer(&firstCalls), newServer(&secondCalls)
	defer first.Close()
	defer second.Close()
	out := filepath.Join(t.TempDir(), "run")
	err = evaluation.Run(t.Context(), first.Client(), set, evaluation.Options{
		Endpoint: first.URL,
		Arms:     []evaluation.Arm{{Name: "current", Model: "auto", Endpoint: first.URL}, {Name: "candidate", Model: "auto", Endpoint: second.URL}},
		Repeats:  1,
		Seed:     1,
		Output:   out,
	})
	if err != nil {
		t.Fatal(err)
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Errorf("arm endpoint counts = %d and %d, want one call per configured endpoint", firstCalls.Load(), secondCalls.Load())
	}
	if _, err := os.Stat(filepath.Join(out, "rater-1.jsonl")); !os.IsNotExist(err) {
		t.Errorf("default run unexpectedly exports review content: %v", err)
	}
	// #nosec G304 -- out is a test-owned temporary directory.
	metrics, err := os.ReadFile(filepath.Join(out, "metrics.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(metrics), `"prompt":"test"`) || strings.Contains(string(metrics), `"completion":"ok"`) {
		t.Errorf("durable metadata leaked prompt or response content: %s", metrics)
	}
}
