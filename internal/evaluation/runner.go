package evaluation

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	mathrand "math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const maxResponseBytes = 16 << 20
const requestTimeout = 2 * time.Minute

// Arm names one routing mode and the OpenAI model value used to invoke it.
type Arm struct {
	Name     string `json:"name"`
	Model    string `json:"model"`
	Endpoint string `json:"-"`
}

// Price is a user-supplied USD price per million tokens.
type Price struct {
	InputUSDPerMillion  float64 `json:"input_usd_per_mtok"`
	OutputUSDPerMillion float64 `json:"output_usd_per_mtok"`
	Free                bool    `json:"free,omitempty"`
}

// Options configures one paired evaluation run.
type Options struct {
	Endpoint             string
	APIKey               string
	Arms                 []Arm
	Repeats              int
	Seed                 int64
	Timeout              time.Duration
	Prices               map[string]Price
	Output               string
	ReviewExportDir      string
	Resume               bool
	RetryFailedIDs       []string
	RulesBenchmark       bool
	CandidateSetSHA256   string
	BenchmarkPilot       bool
	BenchmarkLive        bool
	LiveRunApproved      bool
	CandidateSetApproved bool
	T19RecoveryApproved  bool
	MockPilotInspected   bool
	MockPilotReport      string
	CostCeilingUSD       float64
}

// CostSource identifies how a trial's cost was determined.
type CostSource string

const (
	CostReported  CostSource = "reported"
	CostDeclared  CostSource = "declared"
	CostEstimated CostSource = "estimated"
	CostFree      CostSource = "free"
	CostUnknown   CostSource = "unknown"
)

// Outcome identifies the request result without retaining provider error text.
type Outcome string

const (
	OutcomeSucceeded        Outcome = "succeeded"
	OutcomeFailed           Outcome = "failed"
	OutcomeCancelled        Outcome = "cancelled"
	OutcomeTimeout          Outcome = "timeout"
	OutcomeTransportFailure Outcome = "transport_failure"
	OutcomeProviderError    Outcome = "provider_error"
	OutcomeExhausted        Outcome = "exhausted_candidates"
	OutcomeIncompleteStream Outcome = "incomplete_stream"
)

// Answer is an arm-blinded completion for later human scoring.
type Answer struct {
	AnswerID   string `json:"trial_id"`
	PromptID   string `json:"prompt_id"`
	Category   string `json:"category"`
	Difficulty string `json:"difficulty"`
	Prompt     string `json:"prompt"`
	Completion string `json:"completion"`
}

func (a *Answer) UnmarshalJSON(data []byte) error {
	type wire Answer
	var value struct {
		wire
		LegacyID string `json:"answer_id"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*a = Answer(value.wire)
	if a.AnswerID == "" {
		a.AnswerID = value.LegacyID
	}
	return nil
}

// Assignment maps one blinded answer to its evaluation arm.
type Assignment struct {
	AnswerID       string `json:"trial_id"`
	PromptID       string `json:"prompt_id"`
	Arm            string `json:"arm"`
	Repeat         int    `json:"repeat"`
	RatedForReview bool   `json:"rated_for_review,omitempty"`
}

func (a *Assignment) UnmarshalJSON(data []byte) error {
	type wire Assignment
	var value struct {
		wire
		LegacyID string `json:"answer_id"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*a = Assignment(value.wire)
	if a.AnswerID == "" {
		a.AnswerID = value.LegacyID
	}
	return nil
}

// Trial records non-content timing and cost measurements for one arm/prompt pair.
type Trial struct {
	AnswerID         string     `json:"trial_id"`
	PromptID         string     `json:"prompt_id"`
	Category         string     `json:"category"`
	Difficulty       string     `json:"difficulty"`
	Arm              string     `json:"arm"`
	Repeat           int        `json:"repeat"`
	LatencyMS        int64      `json:"latency_ms"`
	FirstTokenMS     int64      `json:"first_token_ms"`
	PromptTokens     int64      `json:"prompt_tokens"`
	CompletionTokens int64      `json:"completion_tokens"`
	CostUSD          float64    `json:"cost_usd"`
	CostSource       CostSource `json:"cost_source"`
	Outcome          Outcome    `json:"outcome"`
	StartedAtUTC     string     `json:"started_at_utc,omitempty"`
	EndedAtUTC       string     `json:"ended_at_utc,omitempty"`
	HTTPStatusClass  string     `json:"http_status_class,omitempty"`
	RouteReason      string     `json:"route_reason,omitempty"`
	Provider         string     `json:"provider,omitempty"`
	RatedForReview   bool       `json:"rated_for_review,omitempty"`
}

func (t *Trial) UnmarshalJSON(data []byte) error {
	type wire Trial
	var value struct {
		wire
		LegacyID string `json:"answer_id"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*t = Trial(value.wire)
	if t.AnswerID == "" {
		t.AnswerID = value.LegacyID
	}
	return nil
}

// Metrics summarizes one arm and difficulty/category group.
type Metrics struct {
	Arm               string             `json:"arm"`
	Difficulty        string             `json:"difficulty"`
	Category          string             `json:"category"`
	Requests          int                `json:"requests"`
	Successful        int                `json:"successful_requests"`
	Failed            int                `json:"failed_requests"`
	TimedOut          int                `json:"timed_out_requests"`
	Cancelled         int                `json:"cancelled_requests"`
	IncompleteStreams int                `json:"incomplete_streams"`
	Exhausted         int                `json:"exhausted_requests"`
	ZeroCostRequests  int                `json:"zero_cost_requests"`
	SpilloverPicked   int                `json:"spillover_selected"`
	SpilloverOK       int                `json:"spillover_succeeded"`
	StatusClassShare  map[string]float64 `json:"http_status_class_share"`
	MeanCostUSD       float64            `json:"mean_cost_usd"`
	CostCoverage      float64            `json:"known_cost_coverage"`
	CostSourceShare   map[string]float64 `json:"cost_source_share"`
	LatencyP50MS      int64              `json:"latency_p50_ms"`
	LatencyP95MS      int64              `json:"latency_p95_ms"`
	FirstTokenP50MS   int64              `json:"first_token_p50_ms"`
	FirstTokenP95MS   int64              `json:"first_token_p95_ms"`
}

// Report is the content-free machine-readable output of a run.
type Report struct {
	FormatVersion          int         `json:"format_version"`
	PromptSetVersion       int         `json:"prompt_set_version"`
	PromptCount            int         `json:"prompt_count"`
	Categories             []string    `json:"categories"`
	CandidateSet           []Arm       `json:"candidate_set"`
	SettingsSHA256         string      `json:"settings_sha256"`
	CandidateSetSHA256     string      `json:"candidate_set_attestation_sha256,omitempty"`
	RulesBenchmark         bool        `json:"rules_benchmark,omitempty"`
	BenchmarkMode          string      `json:"benchmark_mode,omitempty"`
	LiveRunApproved        bool        `json:"live_run_approved,omitempty"`
	CandidateSetApproved   bool        `json:"candidate_set_approved,omitempty"`
	T19RecoveryApproved    bool        `json:"t19_recovery_approved,omitempty"`
	MockPilotInspected     bool        `json:"mock_pilot_inspected,omitempty"`
	ApprovedCostCeilingUSD float64     `json:"approved_cost_ceiling_usd,omitempty"`
	Seed                   int64       `json:"seed"`
	Environment            Environment `json:"environment"`
	Trials                 []Trial     `json:"trials"`
	Metrics                []Metrics   `json:"metrics"`
}

// Environment records the settings needed to identify and repeat one run.
type Environment struct {
	StartedAtUTC    string            `json:"started_at_utc"`
	GoVersion       string            `json:"go_version"`
	OS              string            `json:"os"`
	Architecture    string            `json:"architecture"`
	EndpointHosts   map[string]string `json:"endpoint_hosts"`
	PromptSetSHA256 string            `json:"prompt_set_sha256"`
	Repeats         int               `json:"repeats"`
	Arms            []safeArm         `json:"arms"`
	Prices          map[string]Price  `json:"prices,omitempty"`
}

type safeArm struct {
	Name  string `json:"name"`
	Model string `json:"model"`
}

func benchmarkMode(options Options) string {
	if options.BenchmarkPilot {
		return "pilot"
	}
	if options.BenchmarkLive {
		return "live"
	}
	return ""
}

// Run executes every prompt/repeat against each arm in a randomized order.
// Content is retained only when an explicit review export directory is supplied.
func Run(ctx context.Context, client *http.Client, set PromptSet, options Options) error {
	if err := validateRun(client, set, options); err != nil {
		return err
	}
	defaultEndpoint, err := parseEndpoint(options.Endpoint)
	if err != nil {
		return err
	}
	if options.Timeout <= 0 {
		options.Timeout = 2 * time.Hour
	}
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	seed := options.Seed
	if seed == 0 {
		var raw [8]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return fmt.Errorf("generate evaluation seed: %w", err)
		}
		seed = int64(binary.LittleEndian.Uint64(raw[:]) & math.MaxInt64)
	}
	startedAt := time.Now().UTC()
	setJSON, err := json.Marshal(set)
	if err != nil {
		return errors.New("encode prompt set fingerprint")
	}
	setHash := sha256.Sum256(setJSON)
	type settingsArm struct {
		Name     string `json:"name"`
		Model    string `json:"model"`
		Endpoint string `json:"endpoint"`
	}
	settingsArms := make([]settingsArm, len(options.Arms))
	for index, arm := range options.Arms {
		endpoint := options.Endpoint
		if arm.Endpoint != "" {
			endpoint = arm.Endpoint
		}
		settingsArms[index] = settingsArm{Name: arm.Name, Model: arm.Model, Endpoint: endpoint}
	}
	settingsData, err := json.Marshal(struct {
		Arms               []settingsArm    `json:"arms"`
		Repeats            int              `json:"repeats"`
		Prices             map[string]Price `json:"prices,omitempty"`
		RulesBenchmark     bool             `json:"rules_benchmark,omitempty"`
		CandidateSetSHA256 string           `json:"candidate_set_sha256,omitempty"`
		BenchmarkPilot     bool             `json:"benchmark_pilot,omitempty"`
		BenchmarkLive      bool             `json:"benchmark_live,omitempty"`
		CostCeilingUSD     float64          `json:"approved_cost_ceiling_usd,omitempty"`
		ReviewSchedule     string           `json:"review_schedule"`
	}{settingsArms, options.Repeats, options.Prices, options.RulesBenchmark, options.CandidateSetSHA256, options.BenchmarkPilot, options.BenchmarkLive, options.CostCeilingUSD, "sha256-seed-prompt-arm-v1"})
	if err != nil {
		return errors.New("encode evaluation settings fingerprint")
	}
	settingsHash := sha256.Sum256(settingsData)
	settingsFingerprint := fmt.Sprintf("%x", settingsHash)
	endpoints := make(map[string]*url.URL, len(options.Arms))
	endpointHosts := make(map[string]string, len(options.Arms))
	for _, arm := range options.Arms {
		endpoint := defaultEndpoint
		if arm.Endpoint != "" {
			endpoint, err = parseEndpoint(arm.Endpoint)
			if err != nil {
				return err
			}
		}
		endpoints[arm.Name] = endpoint
		endpointHosts[arm.Name] = endpoint.Host
	}
	if options.BenchmarkPilot {
		if err := verifyMockEndpoints(ctx, client, endpoints, options.APIKey); err != nil {
			return err
		}
	}
	if options.BenchmarkLive {
		if err := verifyMockPilotReport(ctx, options.MockPilotReport, options.CandidateSetSHA256, options.Arms); err != nil {
			return err
		}
	}
	trials := make([]Trial, 0, len(set.Prompts)*options.Repeats*len(options.Arms))
	answers := make([]Answer, 0, cap(trials))
	assignments := make([]Assignment, 0, cap(trials))
	trialIDs := make(map[string]bool, cap(trials))
	for _, trial := range trials {
		trialIDs[trial.AnswerID] = true
	}
	promptIDs := make(map[string]string, len(set.Prompts))
	for i, prompt := range set.Prompts {
		promptIDs[prompt.ID] = fmt.Sprintf("prompt-%06d", i+1)
	}
	report := Report{
		FormatVersion:          3,
		PromptSetVersion:       set.Version,
		PromptCount:            len(set.Prompts),
		Categories:             categories(set),
		CandidateSet:           append([]Arm(nil), options.Arms...),
		SettingsSHA256:         settingsFingerprint,
		CandidateSetSHA256:     options.CandidateSetSHA256,
		RulesBenchmark:         options.RulesBenchmark,
		BenchmarkMode:          benchmarkMode(options),
		LiveRunApproved:        options.LiveRunApproved,
		CandidateSetApproved:   options.CandidateSetApproved,
		T19RecoveryApproved:    options.T19RecoveryApproved,
		MockPilotInspected:     options.MockPilotInspected,
		ApprovedCostCeilingUSD: options.CostCeilingUSD,
		Seed:                   seed,
		Environment: Environment{
			StartedAtUTC: startedAt.Format(time.RFC3339), GoVersion: runtime.Version(), OS: runtime.GOOS,
			Architecture: runtime.GOARCH, EndpointHosts: endpointHosts, PromptSetSHA256: fmt.Sprintf("%x", setHash),
			Repeats: options.Repeats, Arms: safeArms(options.Arms), Prices: options.Prices,
		},
		Trials:  trials,
		Metrics: []Metrics{},
	}
	metricsPath := filepath.Join(options.Output, "metrics.json")
	// #nosec G304 -- metricsPath is inside the operator-selected evaluation output directory.
	if existing, readErr := os.ReadFile(metricsPath); readErr == nil {
		if !options.Resume {
			return errors.New("evaluation output already contains a run; use resume explicitly")
		}
		if err := json.Unmarshal(existing, &report); err != nil {
			return fmt.Errorf("read resumable evaluation metadata: %w", err)
		}
		if report.Environment.PromptSetSHA256 != fmt.Sprintf("%x", setHash) || report.SettingsSHA256 != settingsFingerprint || report.PromptSetVersion != set.Version || report.PromptCount != len(set.Prompts) {
			return errors.New("resume requires the same corpus digest, candidate set, and settings fingerprint")
		}
		seed = report.Seed
		trials = append(trials, report.Trials...)
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return fmt.Errorf("inspect existing evaluation run: %w", readErr)
	} else if options.Resume {
		return errors.New("cannot resume because the evaluation metadata does not exist")
	}
	if options.ReviewExportDir != "" {
		if err := os.MkdirAll(options.ReviewExportDir, 0o700); err != nil {
			return fmt.Errorf("create review export directory: %w", err)
		}
		if options.Resume {
			answers, err = readReviewWork(ctx, options.ReviewExportDir)
			if err != nil {
				return err
			}
			validIDs := make(map[string]bool, len(trials))
			for _, trial := range trials {
				assignments = append(assignments, Assignment{AnswerID: trial.AnswerID, PromptID: trial.PromptID, Arm: trial.Arm, Repeat: trial.Repeat, RatedForReview: trial.RatedForReview})
				if trial.Outcome == OutcomeSucceeded {
					validIDs[trial.AnswerID] = true
				}
			}
			answers = filterAnswers(answers, validIDs)
		} else {
			entries, err := os.ReadDir(options.ReviewExportDir)
			if err != nil {
				return fmt.Errorf("inspect review export directory: %w", err)
			}
			if len(entries) != 0 {
				return errors.New("review export directory must be empty for a new run")
			}
		}
	}
	// #nosec G404 -- seeded pseudo-random order is for repeatable experiment design, not security.
	rng := mathrand.New(mathrand.NewSource(seed))
	existingByKey := make(map[string]Trial, len(trials))
	failedIDs := make(map[string]bool)
	for _, trial := range trials {
		key := trialKey(trial.PromptID, trial.Arm, trial.Repeat)
		if _, duplicate := existingByKey[key]; duplicate {
			return errors.New("evaluation metadata repeats a prompt, arm, and repeat")
		}
		existingByKey[key] = trial
		if trial.Outcome != OutcomeSucceeded {
			failedIDs[trial.AnswerID] = true
		}
	}
	retryIDs := make(map[string]bool, len(options.RetryFailedIDs))
	for _, id := range options.RetryFailedIDs {
		if id == "" || retryIDs[id] || !failedIDs[id] {
			return errors.New("retry IDs must name distinct failed trials in the saved run")
		}
		retryIDs[id] = true
	}
	for repeat := 1; repeat <= options.Repeats; repeat++ {
		for _, prompt := range set.Prompts {
			order := append([]Arm(nil), options.Arms...)
			rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
			for _, arm := range order {
				promptID := promptIDs[prompt.ID]
				key := trialKey(promptID, arm.Name, repeat)
				previous, exists := existingByKey[key]
				if exists && (previous.Outcome == OutcomeSucceeded || !retryIDs[previous.AnswerID]) {
					continue
				}
				answerID := previous.AnswerID
				if !exists {
					answerID, err = newTrialID(trialIDs)
					if err != nil {
						return err
					}
				}
				attemptStarted := time.Now().UTC()
				trial, completion, err := runTrial(ctx, client, endpoints[arm.Name], options.APIKey, prompt, arm, repeat, answerID, options.Prices)
				attemptEnded := time.Now().UTC()
				if err != nil {
					if trial.AnswerID == "" {
						trial = Trial{AnswerID: answerID, PromptID: promptIDs[prompt.ID], Category: prompt.Category, Difficulty: prompt.Difficulty, Arm: arm.Name, Repeat: repeat, CostSource: CostUnknown}
					}
					if trial.Outcome == "" {
						trial.Outcome = failureOutcome(ctx, err)
					}
					completion = ""
				}
				trial.PromptID = promptIDs[prompt.ID]
				trial.RatedForReview = repeat == selectedReviewRepeat(seed, trial.PromptID, arm.Name, options.Repeats)
				trial.StartedAtUTC = attemptStarted.Format(time.RFC3339Nano)
				trial.EndedAtUTC = attemptEnded.Format(time.RFC3339Nano)
				if err != nil && trial.LatencyMS == 0 {
					trial.LatencyMS = attemptEnded.Sub(attemptStarted).Milliseconds()
				}
				trials = append(trials, trial)
				if exists {
					trials = removeTrial(trials[:len(trials)-1], key)
					trials = append(trials, trial)
				}
				assignments = upsertAssignment(assignments, Assignment{AnswerID: trial.AnswerID, PromptID: trial.PromptID, Arm: trial.Arm, Repeat: trial.Repeat, RatedForReview: trial.RatedForReview})
				if trial.Outcome == OutcomeSucceeded {
					answers = upsertAnswer(answers, Answer{AnswerID: answerID, PromptID: promptIDs[prompt.ID], Category: prompt.Category, Difficulty: prompt.Difficulty, Prompt: prompt.Prompt, Completion: completion})
					if options.ReviewExportDir != "" {
						if err := writeReviewWork(context.WithoutCancel(ctx), options.ReviewExportDir, answers); err != nil {
							return err
						}
					}
				}
				report.Trials = trials
				report.Metrics = summarize(trials)
				if err := writeArtifacts(context.WithoutCancel(ctx), options.Output, report); err != nil {
					return err
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
			}
		}
	}
	for id := range retryIDs {
		var outcome Outcome
		for _, trial := range trials {
			if trial.AnswerID == id {
				outcome = trial.Outcome
				break
			}
		}
		if outcome != OutcomeSucceeded {
			return fmt.Errorf("explicit retry for failed trial %q did not succeed (outcome %q)", id, outcome)
		}
	}
	if options.ReviewExportDir != "" {
		selectedIDs := make(map[string]bool)
		for _, trial := range trials {
			if trial.RatedForReview && trial.Outcome == OutcomeSucceeded {
				selectedIDs[trial.AnswerID] = true
			}
		}
		return writeReviewExport(ctx, options.ReviewExportDir, filterAnswers(answers, selectedIDs), assignments, seed)
	}
	return nil
}

func selectedReviewRepeat(seed int64, promptID, arm string, repeats int) int {
	if repeats < 1 || repeats > 100 {
		return 1
	}
	key := fmt.Sprintf("%d\x00%s\x00%s", seed, promptID, arm)
	digest := sha256.Sum256([]byte(key))
	// #nosec G404 -- deterministic repeat selection is experiment scheduling, not security.
	local := mathrand.New(mathrand.NewSource(int64(binary.LittleEndian.Uint64(digest[:8]) & math.MaxInt64)))
	return local.Intn(repeats) + 1
}

func newTrialID(seen map[string]bool) (string, error) {
	for range 3 {
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", fmt.Errorf("generate evaluation trial ID: %w", err)
		}
		id := fmt.Sprintf("trial-%x", raw[:])
		if !seen[id] {
			seen[id] = true
			return id, nil
		}
	}
	return "", errors.New("generate unique evaluation trial ID")
}

func failureOutcome(ctx context.Context, err error) Outcome {
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return OutcomeCancelled
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return OutcomeTimeout
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		if networkError.Timeout() {
			return OutcomeTimeout
		}
		return OutcomeTransportFailure
	}
	var typed requestFailure
	if errors.As(err, &typed) {
		return typed.outcome
	}
	return OutcomeProviderError
}

type requestFailure struct {
	outcome Outcome
	err     error
}

func (e requestFailure) Error() string { return e.err.Error() }
func (e requestFailure) Unwrap() error { return e.err }

func trialKey(promptID, arm string, repeat int) string {
	return fmt.Sprintf("%s\x00%s\x00%d", promptID, arm, repeat)
}

func upsertAnswer(values []Answer, answer Answer) []Answer {
	for index := range values {
		if values[index].AnswerID == answer.AnswerID {
			values[index] = answer
			return values
		}
	}
	return append(values, answer)
}

func upsertAssignment(values []Assignment, assignment Assignment) []Assignment {
	for index := range values {
		if values[index].AnswerID == assignment.AnswerID {
			values[index] = assignment
			return values
		}
	}
	return append(values, assignment)
}

func filterAnswers(values []Answer, validIDs map[string]bool) []Answer {
	filtered := make([]Answer, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, answer := range values {
		if validIDs[answer.AnswerID] && !seen[answer.AnswerID] {
			filtered = append(filtered, answer)
			seen[answer.AnswerID] = true
		}
	}
	return filtered
}

func directoriesOverlap(first, second string) bool {
	firstAbs, firstErr := filepath.Abs(first)
	secondAbs, secondErr := filepath.Abs(second)
	if firstErr != nil || secondErr != nil {
		return true
	}
	return containsPath(firstAbs, secondAbs) || containsPath(secondAbs, firstAbs)
}

func containsPath(parent, child string) bool {
	relative, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	if err != nil {
		return true
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func removeTrial(trials []Trial, key string) []Trial {
	for i, trial := range trials {
		if trialKey(trial.PromptID, trial.Arm, trial.Repeat) == key {
			return append(trials[:i], trials[i+1:]...)
		}
	}
	return trials
}

func categories(set PromptSet) []string {
	seen := map[string]bool{}
	for _, prompt := range set.Prompts {
		seen[prompt.Category] = true
	}
	result := make([]string, 0, len(seen))
	for category := range seen {
		result = append(result, category)
	}
	sort.Strings(result)
	return result
}

func safeArms(arms []Arm) []safeArm {
	result := make([]safeArm, len(arms))
	for i, arm := range arms {
		result[i] = safeArm{Name: arm.Name, Model: arm.Model}
	}
	return result
}

func validateRun(client *http.Client, set PromptSet, options Options) error {
	if client == nil {
		return errors.New("evaluation HTTP client is required")
	}
	if _, err := url.ParseRequestURI(options.Endpoint); err != nil {
		return errors.New("evaluation endpoint must be a valid URL")
	}
	if len(options.Arms) < 1 || len(options.Arms) > 8 {
		return errors.New("evaluation requires 1 to 8 arms")
	}
	if options.Repeats < 1 || options.Repeats > 100 {
		return errors.New("evaluation repeats must be from 1 to 100")
	}
	if options.Output == "" {
		return errors.New("evaluation output directory is required")
	}
	if options.ReviewExportDir != "" && directoriesOverlap(options.Output, options.ReviewExportDir) {
		return errors.New("review export directory must be separate from the durable output directory")
	}
	seen := make(map[string]bool, len(options.Arms))
	for _, arm := range options.Arms {
		if !label(arm.Name) || strings.TrimSpace(arm.Model) == "" || len(arm.Model) > 256 {
			return errors.New("evaluation arm names and models must be non-empty bounded values")
		}
		if seen[arm.Name] {
			return fmt.Errorf("duplicate evaluation arm %q", arm.Name)
		}
		seen[arm.Name] = true
		if arm.Endpoint != "" {
			if _, err := parseEndpoint(arm.Endpoint); err != nil {
				return err
			}
		}
	}
	for model, price := range options.Prices {
		if strings.TrimSpace(model) == "" || price.InputUSDPerMillion < 0 || price.OutputUSDPerMillion < 0 || math.IsNaN(price.InputUSDPerMillion) || math.IsNaN(price.OutputUSDPerMillion) || math.IsInf(price.InputUSDPerMillion, 0) || math.IsInf(price.OutputUSDPerMillion, 0) || (price.Free && (price.InputUSDPerMillion != 0 || price.OutputUSDPerMillion != 0)) {
			return errors.New("evaluation prices must use non-empty model references and finite non-negative values")
		}
	}
	data, err := json.Marshal(set)
	if err != nil {
		return errors.New("evaluation prompt set cannot be encoded")
	}
	if _, err := LoadPromptSet(bytes.NewReader(data)); err != nil {
		return fmt.Errorf("invalid evaluation prompt set: %w", err)
	}
	if options.RulesBenchmark {
		if err := validateRulesBenchmark(set, options); err != nil {
			return err
		}
	} else if options.BenchmarkPilot || options.BenchmarkLive || options.LiveRunApproved || options.CandidateSetApproved || options.T19RecoveryApproved || options.MockPilotInspected || options.MockPilotReport != "" || options.CostCeilingUSD != 0 || options.CandidateSetSHA256 != "" {
		return errors.New("benchmark options require rules benchmark mode")
	}
	return nil
}

func validateRulesBenchmark(set PromptSet, options Options) error {
	if options.BenchmarkPilot == options.BenchmarkLive {
		return errors.New("rules benchmark requires exactly one of pilot or live mode")
	}
	counts := map[string]int{}
	for _, prompt := range set.Prompts {
		counts[prompt.Difficulty]++
	}
	if options.BenchmarkLive {
		if len(set.Prompts) < 90 || counts["simple"] < 30 || counts["moderate"] < 30 || counts["hard"] < 30 {
			return errors.New("live rules benchmark requires at least 90 prompts and 30 per difficulty")
		}
		if options.Repeats < 3 {
			return errors.New("live rules benchmark requires at least 3 repeats per prompt and arm")
		}
		if !options.LiveRunApproved || !options.CandidateSetApproved || !options.T19RecoveryApproved || !options.MockPilotInspected || options.MockPilotReport == "" || options.CostCeilingUSD <= 0 || math.IsNaN(options.CostCeilingUSD) || math.IsInf(options.CostCeilingUSD, 0) {
			return errors.New("live rules benchmark requires separate run, candidate-set, T19 recovery, and cost-ceiling approvals plus an inspected mock pilot")
		}
	} else if len(set.Prompts) < 3 || counts["simple"] < 1 || counts["moderate"] < 1 || counts["hard"] < 1 {
		return errors.New("mock rules pilot requires at least one prompt per difficulty")
	}
	if options.Seed == 0 {
		return errors.New("rules benchmark requires an explicit non-zero seed")
	}
	if len(options.Arms) != 2 {
		return errors.New("rules benchmark requires current-auto and rules arms")
	}
	arms := make(map[string]Arm, len(options.Arms))
	for _, arm := range options.Arms {
		arms[arm.Name] = arm
	}
	baseline, hasBaseline := arms["current-auto"]
	rules, hasRules := arms["rules"]
	if !hasBaseline || !hasRules || baseline.Model != "auto" || rules.Model != "auto" {
		return errors.New("rules benchmark arms must be current-auto=auto and rules=auto")
	}
	baselineEndpoint, err := effectiveEndpoint(options.Endpoint, baseline.Endpoint)
	if err != nil {
		return err
	}
	rulesEndpoint, err := effectiveEndpoint(options.Endpoint, rules.Endpoint)
	if err != nil {
		return err
	}
	if baselineEndpoint == rulesEndpoint {
		return errors.New("rules benchmark arms must use separate auto endpoints for their routing configurations")
	}
	digest, err := hex.DecodeString(options.CandidateSetSHA256)
	if err != nil || len(digest) != sha256.Size {
		return errors.New("rules benchmark requires the shared candidate-set SHA-256 digest")
	}
	if options.BenchmarkPilot && (options.LiveRunApproved || options.CandidateSetApproved || options.T19RecoveryApproved || options.MockPilotInspected || options.MockPilotReport != "" || options.CostCeilingUSD != 0) {
		return errors.New("mock rules pilot does not accept live-run approvals or a live cost ceiling")
	}
	return nil
}

func effectiveEndpoint(defaultEndpoint, armEndpoint string) (string, error) {
	if armEndpoint == "" {
		endpoint, err := parseEndpoint(defaultEndpoint)
		if err != nil {
			return "", err
		}
		return endpoint.String(), nil
	}
	endpoint, err := parseEndpoint(armEndpoint)
	if err != nil {
		return "", err
	}
	return endpoint.String(), nil
}

func parseEndpoint(value string) (*url.URL, error) {
	endpoint, err := url.Parse(strings.TrimRight(value, "/") + "/v1/chat/completions")
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("evaluation endpoint must be an HTTP URL without credentials, query, or fragment")
	}
	return endpoint, nil
}

func runTrial(ctx context.Context, client *http.Client, endpoint *url.URL, apiKey string, prompt Prompt, arm Arm, repeat int, answerID string, prices map[string]Price) (trial Trial, completion string, retErr error) {
	body := map[string]any{
		"model":          arm.Model,
		"messages":       []map[string]string{{"role": "user", "content": prompt.Prompt}},
		"stream":         true,
		"stream_options": map[string]bool{"include_usage": true},
	}
	for key, raw := range prompt.Settings {
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return Trial{}, "", errors.New("invalid prompt settings")
		}
		body[key] = value
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return Trial{}, "", errors.New("encode evaluation request")
	}
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint.String(), bytes.NewReader(encoded))
	if err != nil {
		return Trial{}, "", errors.New("build evaluation request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Spillway-Cache", "bypass")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return Trial{}, "", fmt.Errorf("send request: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil && retErr == nil {
			retErr = errors.New("close evaluation response body")
		}
	}()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		outcome := OutcomeProviderError
		if strings.Contains(resp.Header.Get("x-spillway-route"), "exhausted") {
			outcome = OutcomeExhausted
		}
		return Trial{AnswerID: answerID, PromptID: prompt.ID, Category: prompt.Category, Difficulty: prompt.Difficulty, Arm: arm.Name, Repeat: repeat, CostSource: CostUnknown, Outcome: outcome, HTTPStatusClass: statusClass(resp.StatusCode), RouteReason: resp.Header.Get("x-spillway-route"), Provider: resp.Header.Get("x-spillway-provider"), LatencyMS: time.Since(started).Milliseconds()}, "", fmt.Errorf("endpoint returned HTTP %d", resp.StatusCode)
	}
	reader := bufio.NewReader(io.LimitReader(resp.Body, maxResponseBytes+1))
	var content strings.Builder
	var promptTokens, completionTokens int64
	var reportedCost float64
	var hasReportedCost bool
	var contentBytes int64
	var firstToken time.Duration
	for {
		line, readErr := reader.ReadString('\n')
		if int64(content.Len())+contentBytes+int64(len(line)) > maxResponseBytes {
			return Trial{}, "", errors.New("evaluation response exceeds 16 MiB")
		}
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			if data == "[DONE]" {
				break
			}
			var event struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
				Usage *struct {
					PromptTokens     int64           `json:"prompt_tokens"`
					CompletionTokens int64           `json:"completion_tokens"`
					Cost             json.RawMessage `json:"cost"`
				} `json:"usage"`
			}
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				return Trial{}, "", errors.New("endpoint returned malformed stream data")
			}
			for _, choice := range event.Choices {
				if choice.Delta.Content != "" {
					if firstToken == 0 {
						firstToken = time.Since(started)
					}
					content.WriteString(choice.Delta.Content)
					contentBytes += int64(len(choice.Delta.Content))
				}
			}
			if event.Usage != nil {
				promptTokens, completionTokens = event.Usage.PromptTokens, event.Usage.CompletionTokens
				if len(event.Usage.Cost) > 0 && string(event.Usage.Cost) != "null" {
					hasReportedCost = json.Unmarshal(event.Usage.Cost, &reportedCost) == nil
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return Trial{AnswerID: answerID, PromptID: prompt.ID, Category: prompt.Category, Difficulty: prompt.Difficulty, Arm: arm.Name, Repeat: repeat, CostSource: CostUnknown, Outcome: OutcomeIncompleteStream, HTTPStatusClass: statusClass(resp.StatusCode), RouteReason: resp.Header.Get("x-spillway-route"), Provider: resp.Header.Get("x-spillway-provider"), LatencyMS: time.Since(started).Milliseconds()}, "", requestFailure{outcome: OutcomeIncompleteStream, err: errors.New("endpoint ended the stream before [DONE]")}
			}
			outcome := failureOutcome(ctx, readErr)
			if outcome == OutcomeProviderError {
				outcome = OutcomeTransportFailure
			}
			return Trial{AnswerID: answerID, PromptID: prompt.ID, Category: prompt.Category, Difficulty: prompt.Difficulty, Arm: arm.Name, Repeat: repeat, CostSource: CostUnknown, Outcome: outcome, HTTPStatusClass: statusClass(resp.StatusCode), RouteReason: resp.Header.Get("x-spillway-route"), Provider: resp.Header.Get("x-spillway-provider"), LatencyMS: time.Since(started).Milliseconds()}, "", requestFailure{outcome: outcome, err: errors.New("read evaluation stream")}
		}
	}
	latency := time.Since(started)
	usageSource := "reported"
	if promptTokens <= 0 || completionTokens < 0 {
		promptTokens = int64(math.Ceil(float64(len(prompt.Prompt)) / 4))
		completionTokens = int64(math.Ceil(float64(contentBytes) / 4))
		usageSource = "estimated"
	}
	cost := reportedCost
	modelRef := arm.Model
	if provider := resp.Header.Get("x-spillway-provider"); provider != "" && resp.Header.Get("x-spillway-model") != "" {
		modelRef = provider + "/" + resp.Header.Get("x-spillway-model")
	}
	price, hasPrice := prices[modelRef]
	var costSource CostSource
	switch {
	case hasReportedCost && cost >= 0 && !math.IsNaN(cost) && !math.IsInf(cost, 0):
		costSource = "reported"
	case hasPrice && price.Free:
		cost, costSource = 0, "free"
	case hasPrice:
		cost = (float64(promptTokens)*price.InputUSDPerMillion + float64(completionTokens)*price.OutputUSDPerMillion) / 1_000_000
		costSource = "declared"
		if usageSource == "estimated" {
			costSource = "estimated"
		}
	default:
		cost, costSource = 0, "unknown"
	}
	return Trial{AnswerID: answerID, PromptID: prompt.ID, Category: prompt.Category, Difficulty: prompt.Difficulty, Arm: arm.Name, Repeat: repeat, LatencyMS: latency.Milliseconds(), FirstTokenMS: firstToken.Milliseconds(), PromptTokens: promptTokens, CompletionTokens: completionTokens, CostUSD: cost, CostSource: costSource, Outcome: OutcomeSucceeded, HTTPStatusClass: statusClass(resp.StatusCode), RouteReason: resp.Header.Get("x-spillway-route"), Provider: resp.Header.Get("x-spillway-provider")}, content.String(), nil
}

func statusClass(status int) string {
	if status < 100 || status > 599 {
		return ""
	}
	return fmt.Sprintf("%dxx", status/100)
}

func routeUsedFallback(reason string) bool {
	if strings.Contains(reason, "fallback") {
		return true
	}
	for _, segment := range strings.Split(reason, ";") {
		segment = strings.TrimSpace(segment)
		if !strings.HasPrefix(segment, "candidate ") {
			continue
		}
		position, _, ok := strings.Cut(strings.TrimPrefix(segment, "candidate "), "/")
		if !ok {
			return false
		}
		return position != "1"
	}
	return false
}

func summarize(trials []Trial) []Metrics {
	type groupKey struct{ arm, difficulty, category string }
	groups := make(map[groupKey][]Trial)
	for _, trial := range trials {
		groups[groupKey{trial.Arm, trial.Difficulty, trial.Category}] = append(groups[groupKey{trial.Arm, trial.Difficulty, trial.Category}], trial)
		groups[groupKey{trial.Arm, trial.Difficulty, "all"}] = append(groups[groupKey{trial.Arm, trial.Difficulty, "all"}], trial)
		groups[groupKey{trial.Arm, "all", trial.Category}] = append(groups[groupKey{trial.Arm, "all", trial.Category}], trial)
		groups[groupKey{trial.Arm, "all", "all"}] = append(groups[groupKey{trial.Arm, "all", "all"}], trial)
	}
	keys := make([]groupKey, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].arm != keys[j].arm {
			return keys[i].arm < keys[j].arm
		}
		if keys[i].difficulty != keys[j].difficulty {
			return keys[i].difficulty < keys[j].difficulty
		}
		return keys[i].category < keys[j].category
	})
	result := make([]Metrics, 0, len(keys))
	for _, key := range keys {
		items := groups[key]
		firstTokens, latencies := make([]int64, 0, len(items)), make([]int64, 0, len(items))
		shares := map[string]float64{"reported": 0, "declared": 0, "estimated": 0, "free": 0, "unknown": 0}
		statusClasses := make(map[string]float64)
		successful, spilloverPicked, spilloverOK := 0, 0, 0
		timedOut, cancelled, incomplete, exhausted, zeroCost := 0, 0, 0, 0, 0
		known := 0
		var total float64
		for _, item := range items {
			latencies = append(latencies, item.LatencyMS)
			firstTokens = append(firstTokens, item.FirstTokenMS)
			shares[string(item.CostSource)]++
			if item.Outcome == OutcomeSucceeded {
				successful++
			}
			switch item.Outcome {
			case OutcomeTimeout:
				timedOut++
			case OutcomeCancelled:
				cancelled++
			case OutcomeIncompleteStream:
				incomplete++
			case OutcomeExhausted:
				exhausted++
			}
			if item.CostSource != CostUnknown && item.CostUSD == 0 {
				zeroCost++
			}
			if routeUsedFallback(item.RouteReason) {
				spilloverPicked++
				if item.Outcome == OutcomeSucceeded {
					spilloverOK++
				}
			}
			if item.HTTPStatusClass != "" {
				statusClasses[item.HTTPStatusClass]++
			}
			if item.CostSource != "unknown" {
				known++
				total += item.CostUSD
			}
		}
		for source := range shares {
			shares[source] /= float64(len(items))
		}
		for status := range statusClasses {
			statusClasses[status] /= float64(len(items))
		}
		coverage := float64(known) / float64(len(items))
		meanCost := 0.0
		if known > 0 {
			meanCost = total / float64(known)
		}
		result = append(result, Metrics{Arm: key.arm, Difficulty: key.difficulty, Category: key.category, Requests: len(items), Successful: successful, Failed: len(items) - successful, TimedOut: timedOut, Cancelled: cancelled, IncompleteStreams: incomplete, Exhausted: exhausted, ZeroCostRequests: zeroCost, SpilloverPicked: spilloverPicked, SpilloverOK: spilloverOK, StatusClassShare: statusClasses, MeanCostUSD: meanCost, CostCoverage: coverage, CostSourceShare: shares, LatencyP50MS: percentile(latencies, .50), LatencyP95MS: percentile(latencies, .95), FirstTokenP50MS: percentile(firstTokens, .50), FirstTokenP95MS: percentile(firstTokens, .95)})
	}
	return result
}

func percentile(values []int64, p float64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[max(0, index)]
}

func writeArtifacts(ctx context.Context, dir string, report Report) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create evaluation output directory: %w", err)
	}
	if err := writeJSON(ctx, filepath.Join(dir, "metrics.json"), report); err != nil {
		return err
	}
	// #nosec G304 -- dir is the operator-selected evaluation artifact directory.
	file, err := os.OpenFile(filepath.Join(dir, "scores.csv"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create blind score sheet: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, file.Close()) }()
	writer := csv.NewWriter(file)
	if err := writer.Write([]string{"rater_id", "trial_id", "correctness_1_to_5", "completeness_1_to_5", "instruction_following_1_to_5", "severe_correctness_failure", "adjudication_note", "adjudicated_severe_correctness_failure"}); err != nil {
		return fmt.Errorf("write blind score sheet: %w", err)
	}
	for _, trial := range report.Trials {
		if err := ctx.Err(); err != nil {
			return err
		}
		if trial.Outcome != OutcomeSucceeded {
			continue
		}
		if err := writer.Write([]string{"", trial.AnswerID, "", "", "", "", "", ""}); err != nil {
			return fmt.Errorf("write blind score sheet: %w", err)
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return fmt.Errorf("write blind score sheet: %w", err)
	}
	return nil
}

func writeReviewExport(ctx context.Context, dir string, answers []Answer, assignments []Assignment, seed int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create review export directory: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("inspect review export directory: %w", err)
	}
	if len(answers) == 0 {
		return errors.New("review export requires at least one successful trial")
	}
	allowed := map[string]bool{".answers-work.jsonl": true, "rater-1.jsonl": true, "rater-2.jsonl": true, "blind-key.json": true, "DELETE_AFTER_RATING.txt": true}
	for _, entry := range entries {
		if !allowed[entry.Name()] {
			return errors.New("review export directory contains an unexpected file")
		}
	}
	first := append([]Answer(nil), answers...)
	second := append([]Answer(nil), answers...)
	// #nosec G404 -- shuffled, independently seeded presentation order is for blinding, not security.
	rng1 := mathrand.New(mathrand.NewSource(seed ^ 0x13579))
	// #nosec G404 -- shuffled, independently seeded presentation order is for blinding, not security.
	rng2 := mathrand.New(mathrand.NewSource(seed ^ 0x24680))
	rng1.Shuffle(len(first), func(i, j int) { first[i], first[j] = first[j], first[i] })
	rng2.Shuffle(len(second), func(i, j int) { second[i], second[j] = second[j], second[i] })
	if len(second) > 1 && sameAnswerOrder(first, second) {
		second[0], second[1] = second[1], second[0]
	}
	for _, item := range []struct {
		name    string
		answers []Answer
	}{{"rater-1.jsonl", first}, {"rater-2.jsonl", second}} {
		if err := writeJSONLines(ctx, filepath.Join(dir, item.name), item.answers); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writeJSON(ctx, filepath.Join(dir, "blind-key.json"), assignments); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writeReviewChecklist(dir); err != nil {
		return fmt.Errorf("write review deletion checklist: %w", err)
	}
	if err := os.Remove(filepath.Join(dir, ".answers-work.jsonl")); err != nil {
		return fmt.Errorf("remove temporary review work file: %w", err)
	}
	return nil
}

func writeReviewWork(ctx context.Context, dir string, answers []Answer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writeReviewChecklist(dir); err != nil {
		return fmt.Errorf("write review deletion checklist: %w", err)
	}
	return writeJSONLines(ctx, filepath.Join(dir, ".answers-work.jsonl"), answers)
}

func writeReviewChecklist(dir string) error {
	checklist := "Temporary blinded evaluation material contains prompts and responses.\n\n" +
		"Deletion checklist after rating:\n" +
		"- [ ] Import the completed ratings into the durable evaluation workflow.\n" +
		"- [ ] Verify ratings reference only opaque trial IDs.\n" +
		"- [ ] Delete rater-1.jsonl and rater-2.jsonl.\n" +
		"- [ ] Delete .answers-work.jsonl if a run was interrupted.\n" +
		"- [ ] Delete blind-key.json.\n" +
		"- [ ] Empty the recycle bin or equivalent if required by local policy.\n"
	return os.WriteFile(filepath.Join(dir, "DELETE_AFTER_RATING.txt"), []byte(checklist), 0o600)
}

func readReviewWork(ctx context.Context, dir string) ([]Answer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, ".answers-work.jsonl")
	// #nosec G304 -- the path is inside the operator-selected temporary review directory.
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return []Answer{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read resumable temporary review material: %w", err)
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	answers := make([]Answer, 0)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var answer Answer
		if err := json.Unmarshal(scanner.Bytes(), &answer); err != nil {
			return nil, fmt.Errorf("decode resumable temporary review material: %w", err)
		}
		answers = append(answers, answer)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read resumable temporary review material: %w", err)
	}
	return answers, nil
}

func sameAnswerOrder(first, second []Answer) bool {
	for i := range first {
		if first[i].AnswerID != second[i].AnswerID {
			return false
		}
	}
	return true
}

func writeJSON(ctx context.Context, path string, value any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode evaluation artifact: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".evaluation-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary evaluation artifact: %w", err)
	}
	tempPath := file.Name()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(tempPath)
		return fmt.Errorf("secure temporary evaluation artifact: %w", err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		_ = os.Remove(tempPath)
		return fmt.Errorf("write evaluation artifact: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(tempPath)
		return fmt.Errorf("sync evaluation artifact: %w", err)
	}
	if err := ctx.Err(); err != nil {
		_ = file.Close()
		_ = os.Remove(tempPath)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("close evaluation artifact: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("replace evaluation artifact: %w", err)
	}
	return nil
}

func writeJSONLines(ctx context.Context, path string, values []Answer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var data bytes.Buffer
	writer := bufio.NewWriter(&data)
	encoder := json.NewEncoder(writer)
	for _, value := range values {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := encoder.Encode(value); err != nil {
			return fmt.Errorf("encode blinded answers: %w", err)
		}
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("encode blinded answers: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".answers-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary blinded answer file: %w", err)
	}
	tempPath := file.Name()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(tempPath)
		return fmt.Errorf("secure temporary blinded answer file: %w", err)
	}
	if _, err := file.Write(data.Bytes()); err != nil {
		_ = file.Close()
		_ = os.Remove(tempPath)
		return fmt.Errorf("write blinded answers: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(tempPath)
		return fmt.Errorf("sync blinded answers: %w", err)
	}
	if err := ctx.Err(); err != nil {
		_ = file.Close()
		_ = os.Remove(tempPath)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("close blinded answers: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("replace blinded answers: %w", err)
	}
	return nil
}
