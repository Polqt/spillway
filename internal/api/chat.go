package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Polqt/spillway/internal/providers"
)

// maxBodyBytes bounds a chat request; 10 MiB fits several base64 images.
const maxBodyBytes = 10 << 20

type chatHandler struct {
	providers         providerSet
	logger            *slog.Logger
	tiers             map[string][]providers.ModelRef
	tracker           candidateTracker
	now               func() time.Time
	bodyReadTimeout   time.Duration
	chunkWriteTimeout time.Duration
}

// chatBody is the part of a chat request Spillway acts on. Model is a
// pointer so a missing model is told apart from an empty one.
type chatBody struct {
	Model    *string             `json:"model"`
	Messages []providers.Message `json:"messages"`
	Stream   bool                `json:"stream"`
	RawBody  json.RawMessage     `json:"-"`
}

func (h *chatHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeChat(w, r, h.bodyReadTimeout)
	if !ok {
		return
	}
	if strings.HasPrefix(*body.Model, "spillway/") {
		tier := strings.TrimPrefix(*body.Model, "spillway/")
		candidates, ok := h.tiers[tier]
		if !ok {
			names := make([]string, 0, len(h.tiers))
			for name := range h.tiers {
				names = append(names, name)
			}
			slices.Sort(names)
			writeError(w, http.StatusNotFound,
				fmt.Sprintf("Tier %q not found. Configured tiers: %s.", tier, strings.Join(names, ", ")),
				typeInvalidRequest, "model_not_found")
			return
		}
		h.serveTier(w, r, tier, candidates, body)
		return
	}
	ref, provider, ok := h.resolve(w, *body.Model)
	if !ok {
		return
	}

	rec := recordFrom(r.Context())
	rec.provider, rec.model = ref.Provider, ref.Model
	// Set before calling the provider so they are present on errors too.
	w.Header().Set("X-Spillway-Provider", ref.Provider)
	w.Header().Set("X-Spillway-Model", ref.Model)

	req := providers.ChatRequest{Model: ref.Model, Messages: body.Messages, RawBody: body.RawBody}
	if body.Stream {
		if err, committed := serveStream(w, r, ref, provider, req, h.chunkWriteTimeout); err != nil && !committed {
			rec.err = err
			writeProviderError(w, r, ref, err)
		}
		return
	}
	resp, err := provider.Chat(r.Context(), req)
	if err != nil {
		rec.err = err
		writeProviderError(w, r, ref, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp.Body)
}

type candidateFailure struct {
	ref    providers.ModelRef
	reason string
	until  time.Time
}

func (h *chatHandler) serveTier(w http.ResponseWriter, r *http.Request, tier string, candidates []providers.ModelRef, body chatBody) {
	rec := recordFrom(r.Context())
	rec.tier = tier
	failures := make([]candidateFailure, 0, len(candidates))
	for _, ref := range candidates {
		if r.Context().Err() != nil {
			return
		}
		rec.err = nil
		if h.tracker != nil {
			if available, until, reason := h.tracker.Available(ref); !available {
				failures = append(failures, candidateFailure{ref: ref, reason: cooldownReason(reason, until, h.now()), until: until})
				continue
			}
		}
		provider, ok := h.providers.Lookup(ref.Provider)
		if !ok {
			// A validated config cannot reach this path, but an incomplete API option should fail safely.
			err := providers.ErrModelNotFound
			reason := candidateReason(err)
			failures = append(failures, candidateFailure{ref: ref, reason: reason})
			h.logCandidateFailure(r, tier, ref, candidateErrorKind(err))
			continue
		}
		clearCandidateHeaders(w)
		req := providers.ChatRequest{Model: ref.Model, Messages: body.Messages, RawBody: body.RawBody}
		rec.attempts++
		var err error
		if body.Stream {
			rec.provider, rec.model = ref.Provider, ref.Model
			w.Header().Set("X-Spillway-Provider", ref.Provider)
			w.Header().Set("X-Spillway-Model", ref.Model)
			var committed bool
			err, committed = serveStream(w, r, ref, provider, req, h.chunkWriteTimeout)
			if committed {
				if h.tracker != nil && r.Context().Err() == nil {
					h.tracker.Observe(ref, err, providers.RateLimit{})
				}
				return
			}
		} else {
			resp, chatErr := provider.Chat(r.Context(), req)
			if chatErr == nil {
				if h.tracker != nil {
					h.tracker.Observe(ref, nil, resp.RateLimit)
				}
				rec.provider, rec.model = ref.Provider, ref.Model
				w.Header().Set("X-Spillway-Provider", ref.Provider)
				w.Header().Set("X-Spillway-Model", ref.Model)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(resp.Body)
				return
			}
			err = chatErr
		}
		if r.Context().Err() != nil {
			return
		}
		if h.tracker != nil {
			h.tracker.Observe(ref, err, providers.RateLimit{})
			if available, until, reason := h.tracker.Available(ref); !available {
				failures = append(failures, candidateFailure{ref: ref, reason: cooldownReason(reason, until, h.now()), until: until})
			} else {
				failures = append(failures, candidateFailure{ref: ref, reason: candidateReason(err)})
			}
		} else {
			failures = append(failures, candidateFailure{ref: ref, reason: candidateReason(err)})
		}
		h.logCandidateFailure(r, tier, ref, candidateErrorKind(err))
		if !isSpillable(err) {
			clearCandidateHeaders(w)
			rec.err = err
			writeProviderError(w, r, ref, err)
			return
		}
	}
	clearCandidateHeaders(w)
	message := exhaustionMessage(tier, failures)
	rec.err = errors.New("no candidate available")
	if retryAfter, allCooling := earliestCooldown(failures, len(candidates), h.now()); allCooling {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeError(w, http.StatusTooManyRequests, message, typeAPI, "rate_limit_exceeded")
		return
	}
	writeError(w, http.StatusServiceUnavailable, message, typeAPI, "no_candidate_available")
}

func cooldownReason(reason string, until, now time.Time) string {
	if reason == "" || until.IsZero() {
		return reason
	}
	remaining := until.Sub(now)
	if remaining < 0 {
		remaining = 0
	}
	seconds := int((remaining + time.Second - 1) / time.Second)
	return fmt.Sprintf("%s (retry in %ds)", reason, seconds)
}

func earliestCooldown(failures []candidateFailure, candidateCount int, now time.Time) (int, bool) {
	if candidateCount == 0 || len(failures) != candidateCount {
		return 0, false
	}
	var earliest time.Time
	for _, failure := range failures {
		if failure.until.IsZero() {
			return 0, false
		}
		if earliest.IsZero() || failure.until.Before(earliest) {
			earliest = failure.until
		}
	}
	remaining := earliest.Sub(now)
	if remaining < 0 {
		remaining = 0
	}
	seconds := int((remaining + time.Second - 1) / time.Second)
	return seconds, true
}

func clearCandidateHeaders(w http.ResponseWriter) {
	w.Header().Del("X-Spillway-Provider")
	w.Header().Del("X-Spillway-Model")
	w.Header().Del("Retry-After")
}

func (h *chatHandler) logCandidateFailure(r *http.Request, tier string, ref providers.ModelRef, kind string) {
	log := h.logger.WarnContext
	if kind == "unauthorized" {
		log = h.logger.ErrorContext
	}
	log(r.Context(), "tier candidate failed", "tier", tier, "provider", ref.Provider, "model", ref.Model, "error_kind", kind)
}

func candidateErrorKind(err error) string {
	switch {
	case errors.Is(err, providers.ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, providers.ErrPaymentRequired):
		return "payment_required"
	case errors.Is(err, providers.ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, providers.ErrModelNotFound):
		return "model_not_found"
	case errors.Is(err, providers.ErrUpstreamUnavailable):
		return "upstream_unavailable"
	case errors.Is(err, providers.ErrBadRequest):
		return "bad_request"
	default:
		return "upstream_error"
	}
}

func candidateReason(err error) string {
	switch candidateErrorKind(err) {
	case "rate_limited":
		return "rate limited"
	case "payment_required":
		return "payment required"
	case "unauthorized":
		return "unauthorized"
	case "model_not_found":
		return "model not found"
	case "upstream_unavailable":
		return "unavailable"
	case "bad_request":
		return "bad request"
	default:
		return "upstream error"
	}
}

func isSpillable(err error) bool {
	return errors.Is(err, providers.ErrRateLimited) || errors.Is(err, providers.ErrPaymentRequired) ||
		errors.Is(err, providers.ErrUnauthorized) || errors.Is(err, providers.ErrModelNotFound) ||
		errors.Is(err, providers.ErrUpstreamUnavailable)
}

func exhaustionMessage(tier string, failures []candidateFailure) string {
	const maxMessageBytes = 1800
	message := fmt.Sprintf("No candidate in tier %q can serve this request: ", tier)
	for i, failure := range failures {
		entry := fmt.Sprintf("%s %s", failure.ref.String(), failure.reason)
		if i > 0 {
			entry = "; " + entry
		}
		if len(message)+len(entry) > maxMessageBytes {
			remaining := maxMessageBytes - len(message)
			if remaining > 3 {
				message += entry[:remaining-3] + "..."
			} else {
				message += "..."
			}
			break
		}
		message += entry
	}
	return message
}

func decodeChat(w http.ResponseWriter, r *http.Request, timeout time.Duration) (chatBody, bool) {
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(timeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		writeError(w, http.StatusInternalServerError, "Request body deadline could not be set.", typeAPI, "internal_error")
		return chatBody{}, false
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err == nil {
		if clearErr := rc.SetReadDeadline(time.Time{}); clearErr != nil && !errors.Is(clearErr, http.ErrNotSupported) {
			err = clearErr
		}
	} else {
		// Do not reuse a connection whose body was incomplete or exceeded its limit.
		r.Close = true
		w.Header().Set("Connection", "close")
		_ = r.Body.Close()
		_ = rc.SetReadDeadline(time.Time{})
	}
	if maxErr := (*http.MaxBytesError)(nil); errors.As(err, &maxErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "Request body is larger than 10 MiB.", typeInvalidRequest, "request_too_large")
		return chatBody{}, false
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "Request body could not be read.", typeInvalidRequest, "invalid_request")
		return chatBody{}, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var body chatBody
	err = dec.Decode(&body)
	if err == nil && !errors.Is(dec.Decode(&struct{}{}), io.EOF) {
		err = errors.New("trailing data after the JSON object")
	}
	if maxErr := (*http.MaxBytesError)(nil); errors.As(err, &maxErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "Request body is larger than 10 MiB.", typeInvalidRequest, "request_too_large")
		return chatBody{}, false
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "Request body must be one JSON object: "+err.Error(), typeInvalidRequest, "invalid_request")
		return chatBody{}, false
	}
	if body.Model == nil || *body.Model == "" {
		writeError(w, http.StatusBadRequest, `"model" is required, as provider/model such as mock/echo.`, typeInvalidRequest, "invalid_request")
		return chatBody{}, false
	}
	if len(body.Messages) == 0 {
		writeError(w, http.StatusBadRequest, `"messages" must be a non-empty array.`, typeInvalidRequest, "invalid_request")
		return chatBody{}, false
	}
	body.RawBody = raw
	return body, true
}

func (h *chatHandler) resolve(w http.ResponseWriter, model string) (providers.ModelRef, providers.Provider, bool) {
	if model == "auto" {
		writeError(w, http.StatusNotFound,
			fmt.Sprintf("Virtual model %q requires the router, which arrives in v0.3. Send a provider/model reference, or see GET /v1/models.", model),
			typeInvalidRequest, "model_not_found")
		return providers.ModelRef{}, nil, false
	}
	ref, err := providers.ParseModelRef(model)
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Model %q not found. Send a model reference as provider/model, such as mock/echo, or see GET /v1/models.", model), typeInvalidRequest, "model_not_found")
		return providers.ModelRef{}, nil, false
	}
	p, ok := h.providers.Lookup(ref.Provider)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Model %q not found: provider %q is not configured. Send a configured provider/model reference, or see GET /v1/models.", model, ref.Provider), typeInvalidRequest, "model_not_found")
		return providers.ModelRef{}, nil, false
	}
	return ref, p, true
}

// writeProviderError answers a provider error that arrived before any of
// the response was sent. A client that is gone gets nothing; the access log
// still records the error.
func writeProviderError(w http.ResponseWriter, r *http.Request, ref providers.ModelRef, err error) {
	if r.Context().Err() != nil {
		return
	}
	status, detail := providerError(ref, err)
	var retry providers.RetryAfterError
	if errors.As(err, &retry) && retry.After > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.After.Seconds())))
	}
	writeErrorDetail(w, status, detail)
}

// providerError maps a provider error to an HTTP status and the OpenAI error
// shape. The client never sees the provider's error text; the access log
// records it.
func providerError(ref providers.ModelRef, err error) (int, errorDetail) {
	switch {
	case errors.Is(err, providers.ErrPaymentRequired):
		return http.StatusTooManyRequests, errorDetail{Message: fmt.Sprintf("Provider %q has insufficient quota; add credit or use another model.", ref.Provider), Type: "insufficient_quota", Code: "insufficient_quota"}
	case errors.Is(err, providers.ErrModelNotFound):
		return http.StatusNotFound, errorDetail{
			Message: fmt.Sprintf("Model %q not found on provider %q.", ref.String(), ref.Provider),
			Type:    typeInvalidRequest,
			Code:    "model_not_found",
		}
	case errors.Is(err, providers.ErrUpstreamUnavailable):
		return http.StatusBadGateway, errorDetail{
			Message: fmt.Sprintf("Provider %q is unavailable; retry later or use another model.", ref.Provider),
			Type:    typeAPI,
			Code:    "upstream_unavailable",
		}
	case errors.Is(err, providers.ErrRateLimited):
		return http.StatusTooManyRequests, errorDetail{Message: fmt.Sprintf("Provider %q is rate limited; retry later or use another model.", ref.Provider), Type: typeAPI, Code: "rate_limit_exceeded"}
	case errors.Is(err, providers.ErrUnauthorized):
		return http.StatusBadGateway, errorDetail{Message: fmt.Sprintf("Provider %q rejected its configured key.", ref.Provider), Type: typeAPI, Code: "upstream_unauthorized"}
	case errors.Is(err, providers.ErrBadRequest):
		return http.StatusBadRequest, errorDetail{Message: fmt.Sprintf("Provider %q rejected the request.", ref.Provider), Type: typeInvalidRequest, Code: "provider_bad_request"}
	default:
		return http.StatusInternalServerError, internalError()
	}
}
