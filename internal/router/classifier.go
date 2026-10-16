package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	classifierTimeout   = 3 * time.Second
	classifierInputMax  = 4096
	classifierOutputMax = 1024
)

// DifficultyClassifier chooses one closed routing difficulty for a request.
type DifficultyClassifier interface {
	Classify(ctx context.Context, body []byte) (string, error)
}

// OllamaClassifier calls the explicitly configured local Ollama generate API.
type OllamaClassifier struct {
	endpoint string
	model    string
	client   *http.Client
}

// NewOllamaClassifier constructs a bounded classifier for a pinned local model.
func NewOllamaClassifier(baseURL, model string) (*OllamaClassifier, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") || !isLoopbackHost(parsed.Hostname()) {
		return nil, errors.New("router classifier: Ollama base URL must be a configured loopback URL")
	}
	tagSeparator := strings.LastIndexByte(model, ':')
	if tagSeparator <= 0 || tagSeparator == len(model)-1 || strings.HasSuffix(strings.ToLower(model), ":latest") {
		return nil, errors.New("router classifier: model must include a pinned tag")
	}
	root := strings.TrimRight(baseURL, "/")
	root = strings.TrimSuffix(root, "/v1")
	endpoint := root + "/api/generate"
	return &OllamaClassifier{
		endpoint: endpoint,
		model:    model,
		client: &http.Client{
			Timeout: classifierTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

type ollamaRequest struct {
	Model   string         `json:"model"`
	Prompt  string         `json:"prompt"`
	Stream  bool           `json:"stream"`
	Format  map[string]any `json:"format"`
	Options ollamaOptions  `json:"options"`
}

type ollamaOptions struct {
	NumPredict int `json:"num_predict"`
}

type ollamaResponse struct {
	Response string `json:"response"`
}

// Classify sends only a bounded text summary and accepts a strict JSON label.
func (c *OllamaClassifier) Classify(ctx context.Context, body []byte) (difficulty string, retErr error) {
	prompt, err := classificationPrompt(body)
	if err != nil {
		return "", err
	}
	requestBody, err := json.Marshal(ollamaRequest{
		Model: c.model, Prompt: prompt, Stream: false,
		Format: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"difficulty": map[string]any{"type": "string", "enum": []string{"simple", "moderate", "hard"}},
				"reason":     map[string]any{"type": "string", "enum": []string{"classified", "uncertain"}},
			},
			"required": []string{"difficulty", "reason"}, "additionalProperties": false,
		},
		Options: ollamaOptions{NumPredict: 32},
	})
	if err != nil {
		return "", fmt.Errorf("encode classifier request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(requestBody))
	if err != nil {
		return "", fmt.Errorf("build classifier request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("call local classifier: %w", err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil && retErr == nil {
			retErr = fmt.Errorf("close local classifier response: %w", err)
		}
	}()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("local classifier returned status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, classifierOutputMax+1))
	if err != nil {
		return "", fmt.Errorf("read local classifier response: %w", err)
	}
	if len(data) > classifierOutputMax {
		return "", errors.New("local classifier response exceeds size limit")
	}
	var envelope ollamaResponse
	if err := json.Unmarshal(data, &envelope); err != nil {
		return "", errors.New("local classifier response is malformed")
	}
	classified, reason, err := decodeClassifierOutput(envelope.Response)
	if err != nil {
		return "", err
	}
	if reason == "uncertain" {
		return "", errors.New("local classifier is uncertain")
	}
	if reason != "classified" {
		return "", errors.New("local classifier output has an unknown reason")
	}
	if !validDifficulty(classified) {
		return "", errors.New("local classifier output has an unknown difficulty")
	}
	return classified, nil
}

func decodeClassifierOutput(output string) (string, string, error) {
	decoder := json.NewDecoder(strings.NewReader(output))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return "", "", errors.New("local classifier output does not match the required schema")
	}
	seen := make(map[string]bool, 2)
	var difficulty, reason string
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return "", "", errors.New("local classifier output does not match the required schema")
		}
		key, ok := token.(string)
		if !ok || (key != "difficulty" && key != "reason") || seen[key] {
			return "", "", errors.New("local classifier output does not match the required schema")
		}
		seen[key] = true
		var value *string
		if key == "difficulty" {
			value = &difficulty
		} else {
			value = &reason
		}
		if err := decoder.Decode(value); err != nil {
			return "", "", errors.New("local classifier output does not match the required schema")
		}
	}
	if _, err := decoder.Token(); err != nil || len(seen) != 2 {
		return "", "", errors.New("local classifier output does not match the required schema")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return "", "", errors.New("local classifier output contains trailing data")
	}
	return difficulty, reason, nil
}

type classifierMessage struct {
	Content json.RawMessage `json:"content"`
}

type classifierBody struct {
	Messages []classifierMessage `json:"messages"`
}

func classificationPrompt(body []byte) (string, error) {
	var request classifierBody
	if err := json.Unmarshal(body, &request); err != nil {
		return "", errors.New("classifier request is malformed")
	}
	var text strings.Builder
	for _, message := range request.Messages {
		var content string
		if json.Unmarshal(message.Content, &content) == nil {
			writeClassifierText(&text, content)
		} else {
			var parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(message.Content, &parts) == nil {
				for _, part := range parts {
					if part.Type == "text" {
						writeClassifierText(&text, part.Text)
					}
				}
			}
		}
		if text.Len() < classifierInputMax {
			text.WriteByte('\n')
		}
		if text.Len() == classifierInputMax {
			break
		}
	}
	input := text.String()
	return "Classify the request difficulty as simple, moderate, or hard. Set reason to classified when confident or uncertain when not. Return only the required JSON object.\nRequest text:\n" + input, nil
}

func writeClassifierText(destination *strings.Builder, value string) {
	remaining := classifierInputMax - destination.Len()
	if remaining <= 0 {
		return
	}
	if len(value) > remaining {
		value = value[:remaining]
	}
	destination.WriteString(value)
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
