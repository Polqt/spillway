package evaluation

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	mathrand "math/rand"
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
	Endpoint string
	APIKey   string
	Arms     []Arm
	Repeats  int
	Seed     int64
	Timeout  time.Duration
	Prices   map[string]Price
	Output   string
}

// Answer is an arm-blinded completion for later human scoring.
type Answer struct {
	AnswerID   string `json:"answer_id"`
	PromptID   string `json:"prompt_id"`
	Category   string `json:"category"`
	Difficulty string `json:"difficulty"`
	Completion string `json:"completion"`
}

// Assignment maps one blinded answer to its evaluation arm.
type Assignment struct {
	AnswerID string `json:"answer_id"`
	PromptID string `json:"prompt_id"`
	Arm      string `json:"arm"`
	Repeat   int    `json:"repeat"`
}

// Trial records non-content timing and cost measurements for one arm/prompt pair.
type Trial struct {
	AnswerID         string  `json:"answer_id"`
	PromptID         string  `json:"prompt_id"`
	Category         string  `json:"category"`
	Difficulty       string  `json:"difficulty"`
	Arm              string  `json:"arm"`
	Repeat           int     `json:"repeat"`
	LatencyMS        int64   `json:"latency_ms"`
	FirstTokenMS     int64   `json:"first_token_ms"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	CostSource       string  `json:"cost_source"`
}

// Metrics summarizes one arm and difficulty/category group.
type Metrics struct {
	Arm             string             `json:"arm"`
	Difficulty      string             `json:"difficulty"`
	Category        string             `json:"category"`
	Requests        int                `json:"requests"`
	MeanCostUSD     float64            `json:"mean_cost_usd"`
	CostCoverage    float64            `json:"known_cost_coverage"`
	CostSourceShare map[string]float64 `json:"cost_source_share"`
	LatencyP50MS    int64              `json:"latency_p50_ms"`
	LatencyP95MS    int64              `json:"latency_p95_ms"`
	FirstTokenP50MS int64              `json:"first_token_p50_ms"`
	FirstTokenP95MS int64              `json:"first_token_p95_ms"`
}

// Report is the content-free machine-readable output of a run.
type Report struct {
	FormatVersion    int         `json:"format_version"`
	PromptSetVersion int         `json:"prompt_set_version"`
	Seed             int64       `json:"seed"`
	Environment      Environment `json:"environment"`
	Trials           []Trial     `json:"trials"`
	Metrics          []Metrics   `json:"metrics"`
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
	Arms            []Arm             `json:"arms"`
	Prices          map[string]Price  `json:"prices,omitempty"`
}

// Run executes every prompt/repeat against each arm in a randomized order.
// Completion text is written only to the blinded answer artifact.
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
	// #nosec G404 -- seeded pseudo-random order is for repeatable experiment design, not security.
	rng := mathrand.New(mathrand.NewSource(seed))
	trials := make([]Trial, 0, len(set.Prompts)*options.Repeats*len(options.Arms))
	answers := make([]Answer, 0, cap(trials))
	assignments := make([]Assignment, 0, cap(trials))
	answerNumber := 0
	for repeat := 1; repeat <= options.Repeats; repeat++ {
		for _, prompt := range set.Prompts {
			order := append([]Arm(nil), options.Arms...)
			rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
			for _, arm := range order {
				answerNumber++
				answerID := fmt.Sprintf("answer-%06d", answerNumber)
				trial, completion, err := runTrial(ctx, client, endpoints[arm.Name], options.APIKey, prompt, arm, repeat, answerID, options.Prices)
				if err != nil {
					return fmt.Errorf("evaluation request failed for arm %s, prompt %s, repeat %d: %w", arm.Name, prompt.ID, repeat, err)
				}
				trials = append(trials, trial)
				answers = append(answers, Answer{AnswerID: answerID, PromptID: prompt.ID, Category: prompt.Category, Difficulty: prompt.Difficulty, Completion: completion})
				assignments = append(assignments, Assignment{AnswerID: answerID, PromptID: prompt.ID, Arm: arm.Name, Repeat: repeat})
			}
		}
	}
	metrics := summarize(trials)
	report := Report{
		FormatVersion:    1,
		PromptSetVersion: set.Version,
		Seed:             seed,
		Environment: Environment{
			StartedAtUTC: startedAt.Format(time.RFC3339), GoVersion: runtime.Version(), OS: runtime.GOOS,
			Architecture: runtime.GOARCH, EndpointHosts: endpointHosts, PromptSetSHA256: fmt.Sprintf("%x", setHash),
			Repeats: options.Repeats, Arms: append([]Arm(nil), options.Arms...), Prices: options.Prices,
		},
		Trials:  trials,
		Metrics: metrics,
	}
	return writeArtifacts(options.Output, report, answers, assignments)
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
	return nil
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
		return Trial{}, "", fmt.Errorf("endpoint returned HTTP %d", resp.StatusCode)
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
				return Trial{}, "", errors.New("endpoint ended the stream before [DONE]")
			}
			return Trial{}, "", errors.New("read evaluation stream")
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
	var costSource string
	switch {
	case hasPrice && price.Free:
		cost, costSource = 0, "free"
	case hasReportedCost && cost >= 0 && !math.IsNaN(cost) && !math.IsInf(cost, 0):
		costSource = "reported"
	case hasPrice:
		cost = (float64(promptTokens)*price.InputUSDPerMillion + float64(completionTokens)*price.OutputUSDPerMillion) / 1_000_000
		costSource = "declared"
		if usageSource == "estimated" {
			costSource = "estimated"
		}
	default:
		cost, costSource = 0, "unknown"
	}
	return Trial{AnswerID: answerID, PromptID: prompt.ID, Category: prompt.Category, Difficulty: prompt.Difficulty, Arm: arm.Name, Repeat: repeat, LatencyMS: latency.Milliseconds(), FirstTokenMS: firstToken.Milliseconds(), PromptTokens: promptTokens, CompletionTokens: completionTokens, CostUSD: cost, CostSource: costSource}, content.String(), nil
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
		known := 0
		var total float64
		for _, item := range items {
			latencies = append(latencies, item.LatencyMS)
			firstTokens = append(firstTokens, item.FirstTokenMS)
			shares[item.CostSource]++
			if item.CostSource != "unknown" {
				known++
				total += item.CostUSD
			}
		}
		for source := range shares {
			shares[source] /= float64(len(items))
		}
		coverage := float64(known) / float64(len(items))
		meanCost := 0.0
		if known > 0 {
			meanCost = total / float64(known)
		}
		result = append(result, Metrics{Arm: key.arm, Difficulty: key.difficulty, Category: key.category, Requests: len(items), MeanCostUSD: meanCost, CostCoverage: coverage, CostSourceShare: shares, LatencyP50MS: percentile(latencies, .50), LatencyP95MS: percentile(latencies, .95), FirstTokenP50MS: percentile(firstTokens, .50), FirstTokenP95MS: percentile(firstTokens, .95)})
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

func writeArtifacts(dir string, report Report, answers []Answer, assignments []Assignment) (retErr error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create evaluation output directory: %w", err)
	}
	if err := writeJSON(filepath.Join(dir, "metrics.json"), report); err != nil {
		return err
	}
	if err := writeJSONLines(filepath.Join(dir, "answers.jsonl"), answers); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "blind-key.json"), assignments); err != nil {
		return err
	}
	// #nosec G304 -- dir is the operator-selected evaluation artifact directory.
	file, err := os.OpenFile(filepath.Join(dir, "scores.csv"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create blind score sheet: %w", err)
	}
	defer func() {
		retErr = errors.Join(retErr, file.Close())
	}()
	writer := csv.NewWriter(file)
	if err := writer.Write([]string{"answer_id", "correctness_1_to_5", "completeness_1_to_5", "instruction_following_1_to_5", "severe_correctness_failure", "adjudication_note"}); err != nil {
		return fmt.Errorf("write blind score sheet: %w", err)
	}
	for _, answer := range answers {
		if err := writer.Write([]string{answer.AnswerID, "", "", "", "", ""}); err != nil {
			return fmt.Errorf("write blind score sheet: %w", err)
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return fmt.Errorf("write blind score sheet: %w", err)
	}
	return nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode evaluation artifact: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write evaluation artifact: %w", err)
	}
	return nil
}

func writeJSONLines(path string, values []Answer) error {
	var data bytes.Buffer
	writer := bufio.NewWriter(&data)
	encoder := json.NewEncoder(writer)
	for _, value := range values {
		if err := encoder.Encode(value); err != nil {
			return fmt.Errorf("encode blinded answers: %w", err)
		}
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("encode blinded answers: %w", err)
	}
	if err := os.WriteFile(path, data.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write blinded answers: %w", err)
	}
	return nil
}
