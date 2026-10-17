package evaluation_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Polqt/spillway/internal/evaluation"
)

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
	err = evaluation.Run(t.Context(), server.Client(), set, evaluation.Options{
		Endpoint: server.URL,
		Arms:     []evaluation.Arm{{Name: "current", Model: "auto"}, {Name: "candidate", Model: "spillway/simple"}},
		Repeats:  2,
		Seed:     17,
		Output:   out,
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
	if strings.Contains(string(metrics), "private prompt") || len(report.Trials) != 8 || report.Trials[0].CostSource != "reported" {
		t.Errorf("metrics artifact leaks content or has incorrect trial data: %#v", report)
	}
	// #nosec G304 -- out is a test-owned temporary directory.
	answers, err := os.ReadFile(filepath.Join(out, "answers.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(answers), "private prompt") || strings.Contains(string(answers), `"arm"`) || !strings.Contains(string(answers), "answer-auto") {
		t.Errorf("answers should contain opaque responses without prompt text or arm names: %s", answers)
	}
	// #nosec G304 -- out is a test-owned temporary directory.
	key, err := os.ReadFile(filepath.Join(out, "blind-key.json"))
	if err != nil {
		t.Fatal(err)
	}
	var assignments []evaluation.Assignment
	if err := json.Unmarshal(key, &assignments); err != nil {
		t.Fatalf("decode blind key: %v", err)
	}
	if len(assignments) != 8 || assignments[0].Arm == "" {
		t.Errorf("blind key has invalid assignments: %#v", assignments)
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
	newServer := func(counter *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			counter.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"}}]}`)
			_, _ = fmt.Fprintln(w, "data: [DONE]")
		}))
	}
	first, second := newServer(&firstCalls), newServer(&secondCalls)
	defer first.Close()
	defer second.Close()
	err = evaluation.Run(t.Context(), first.Client(), set, evaluation.Options{
		Endpoint: first.URL,
		Arms:     []evaluation.Arm{{Name: "current", Model: "auto", Endpoint: first.URL}, {Name: "candidate", Model: "auto", Endpoint: second.URL}},
		Repeats:  1,
		Seed:     1,
		Output:   filepath.Join(t.TempDir(), "run"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Errorf("arm endpoint counts = %d and %d, want one call per configured endpoint", firstCalls.Load(), secondCalls.Load())
	}
}
