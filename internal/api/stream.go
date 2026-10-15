package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/router"
	"github.com/Polqt/spillway/internal/store"
)

type streamRateLimiter interface {
	RateLimit() providers.RateLimit
}

// serveStream relays a provider stream as server-sent events, one flushed
// "data:" event per chunk, ending with "data: [DONE]". Headers are sent with
// the first chunk, so a failure before it is still a plain HTTP error.
// serveStream marks whether response bytes were committed, so callers can
// spill before the first chunk and still observe upstream failures afterward.
func serveStream(w http.ResponseWriter, r *http.Request, ref providers.ModelRef, provider providers.Provider, req providers.ChatRequest, writeTimeout time.Duration, captureUsage, stripUsage bool) (committed bool, rateLimit providers.RateLimit, err error) {
	rec := recordFrom(r.Context())
	rec.setStream()
	stream, err := provider.ChatStream(r.Context(), req)
	if err != nil {
		rec.err = err
		if captureUsage {
			estimateStreamUsage(req.RawBody, 0, &rec.usage)
		}
		return false, providers.RateLimit{}, err
	}
	var reported *providers.Usage
	var relayedContentBytes int64
	if captureUsage {
		defer func() {
			if reported != nil {
				rec.usage.PromptTokens = int64(reported.PromptTokens)
				rec.usage.CompletionTokens = int64(reported.CompletionTokens)
				rec.usage.UsageSource = "reported"
				return
			}
			estimateStreamUsage(req.RawBody, relayedContentBytes, &rec.usage)
		}()
	}
	if rateLimiter, ok := stream.(streamRateLimiter); ok {
		rateLimit = rateLimiter.RateLimit()
	}
	defer func() {
		if err := stream.Close(); err != nil && rec.err == nil {
			rec.err = fmt.Errorf("close provider stream: %w", err)
		}
	}()

	rc := http.NewResponseController(w)
	started := false
	for {
		chunk, err := stream.Recv()
		if err != nil && !errors.Is(err, io.EOF) {
			rec.err = err
			if !started {
				return false, rateLimit, err
			}
			if r.Context().Err() == nil {
				writeStreamError(w, rc, ref, err, writeTimeout)
			}
			return true, rateLimit, err
		}
		if captureUsage && chunk.Usage != nil {
			usage := *chunk.Usage
			reported = &usage
		}
		choices, contentBytes := streamChunkContent(chunk.Data)
		if stripUsage && chunk.Usage != nil {
			if choices == 0 {
				continue
			}
			chunk.Data = stripStreamUsage(chunk.Data)
		}
		if !started {
			startEventStream(w)
			started = true
		}
		if errors.Is(err, io.EOF) {
			if err := writeEvent(w, rc, []byte("[DONE]"), writeTimeout); err != nil {
				rec.err = err
			}
			return true, rateLimit, nil
		}
		if err := writeEvent(w, rc, chunk.Data, writeTimeout); err != nil {
			rec.err = err
			return true, rateLimit, nil
		}
		if captureUsage {
			relayedContentBytes += int64(contentBytes)
		}
	}
}

func stripStreamUsage(data []byte) []byte {
	var chunk map[string]json.RawMessage
	if err := json.Unmarshal(data, &chunk); err != nil || chunk == nil {
		return data
	}
	delete(chunk, "usage")
	stripped, err := json.Marshal(chunk)
	if err != nil {
		return data
	}
	return stripped
}

func streamUsageRequest(req providers.ChatRequest, capture bool) (providers.ChatRequest, bool) {
	if !capture {
		return req, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(req.RawBody, &fields); err != nil || fields == nil {
		return req, false
	}
	var options map[string]json.RawMessage
	if len(fields["stream_options"]) > 0 && json.Unmarshal(fields["stream_options"], &options) != nil {
		return req, false
	}
	var include bool
	if json.Unmarshal(options["include_usage"], &include) == nil && include {
		return req, false
	}
	if options == nil {
		options = make(map[string]json.RawMessage)
	}
	options["include_usage"] = json.RawMessage("true")
	encodedOptions, err := json.Marshal(options)
	if err != nil {
		return req, false
	}
	fields["stream_options"] = encodedOptions
	encodedBody, err := json.Marshal(fields)
	if err != nil {
		return req, false
	}
	req.RawBody = encodedBody
	return req, true
}

func streamChunkContent(data []byte) (choices, contentBytes int) {
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content       json.RawMessage `json:"content"`
				Refusal       string          `json:"refusal"`
				Reasoning     string          `json:"reasoning"`
				ReasoningText string          `json:"reasoning_content"`
				ToolCalls     []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &chunk) != nil {
		return -1, 0
	}
	for _, choice := range chunk.Choices {
		contentBytes += contentByteCount(choice.Delta.Content) + len(choice.Delta.Refusal) + len(choice.Delta.Reasoning) + len(choice.Delta.ReasoningText)
		for _, toolCall := range choice.Delta.ToolCalls {
			contentBytes += len(toolCall.Function.Arguments)
		}
	}
	return len(chunk.Choices), contentBytes
}

func contentByteCount(content json.RawMessage) int {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return len(text)
	}
	var parts []json.RawMessage
	if json.Unmarshal(content, &parts) == nil {
		contentBytes := 0
		for _, rawPart := range parts {
			var part struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(rawPart, &part) == nil && part.Type == "text" {
				contentBytes += len(part.Text)
			} else {
				contentBytes += len(rawPart)
			}
		}
		return contentBytes
	}
	if len(content) == 0 || string(content) == "null" {
		return 0
	}
	return len(content)
}

func estimateStreamUsage(body []byte, contentBytes int64, usage *store.Request) {
	promptTokens := int64(0)
	if requirements, err := router.Detect(body); err == nil {
		promptTokens = int64(requirements.EstimatedTokens) * 2
	}
	usage.PromptTokens = promptTokens
	usage.CompletionTokens = (contentBytes + 2) / 3
	usage.UsageSource = "estimated"
}

func startEventStream(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	// Stops reverse proxies such as nginx from buffering the events.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

// writeStreamError ends a stream that has already sent chunks. Spillover is
// no longer possible, so the client gets an OpenAI error event and no
// [DONE], which keeps the truncation visible.
func writeStreamError(w http.ResponseWriter, rc *http.ResponseController, ref providers.ModelRef, err error, timeout time.Duration) {
	_, detail := providerError(ref, err)
	// The stream ends here either way; a failed write has nothing left to stop.
	_ = writeEvent(w, rc, errorJSON(detail), timeout)
}

func writeEvent(w http.ResponseWriter, rc *http.ResponseController, data []byte, timeout time.Duration) error {
	if err := rc.SetWriteDeadline(time.Now().Add(timeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return fmt.Errorf("set client write deadline: %w", err)
	}
	_, writeErr := fmt.Fprintf(w, "data: %s\n\n", data)
	if writeErr == nil {
		writeErr = rc.Flush()
	}
	clearErr := rc.SetWriteDeadline(time.Time{})
	if writeErr != nil {
		return fmt.Errorf("write event to client: %w", writeErr)
	}
	if clearErr != nil && !errors.Is(clearErr, http.ErrNotSupported) {
		return fmt.Errorf("clear client write deadline: %w", clearErr)
	}
	return nil
}
