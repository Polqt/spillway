package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Polqt/spillway/internal/budget"
	"github.com/Polqt/spillway/internal/cache"
	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/router"
)

// maxBodyBytes bounds a chat request; 10 MiB fits several base64 images.
const maxBodyBytes = 10 << 20

type chatHandler struct {
	providers          providerSet
	logger             *slog.Logger
	router             Planner
	tracker            candidateTracker
	now                func() time.Time
	bodyReadTimeout    time.Duration
	chunkWriteTimeout  time.Duration
	captureStreamUsage bool
	pricer             attemptPricer
	gate               *budget.Gate
	exactCache         *cache.Cache
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
	requestBypass := bypassCache(r)
	if h.exactCache != nil {
		if requestBypass {
			recordFrom(r.Context()).usage.Cache = "bypass"
			w.Header().Set("X-Spillway-Cache", "bypass")
		} else {
			recordFrom(r.Context()).usage.Cache = "miss"
			w.Header().Set("X-Spillway-Cache", "miss")
		}
	}
	body, ok := decodeChat(w, r, h.bodyReadTimeout)
	if !ok {
		return
	}
	recordFrom(r.Context()).setRequestedModel(*body.Model)
	recordFrom(r.Context()).configure(h.pricer, body.RawBody, h.gate, h.now)
	rec := recordFrom(r.Context())
	cacheBypass := body.Stream || requestBypass
	if h.exactCache != nil {
		if cacheBypass {
			rec.usage.Cache = "bypass"
			w.Header().Set("X-Spillway-Cache", "bypass")
		} else {
			rec.usage.Cache = "miss"
			w.Header().Set("X-Spillway-Cache", "miss")
		}
	}
	plan, virtual, err := h.router.Plan(*body.Model, body.RawBody)
	if err != nil {
		rec := recordFrom(r.Context())
		rec.addPlanSkips(plan.Skipped)
		rec.usage.ErrorKind = setPlanErrorKind(err)
		setRoute(w, r, planErrorRoute(*body.Model, plan))
		h.writePlanError(w, *body.Model, err)
		return
	}
	if h.exactCache != nil && !cacheBypass {
		if entry, ok := h.exactCache.Get(rec.clientApp, body.RawBody); ok {
			rec.usage.Cache = "hit"
			rec.usage.PromptTokens = entry.PromptTokens
			rec.usage.CompletionTokens = entry.CompletionTokens
			rec.usage.UsageSource = entry.UsageSource
			rec.usage.CostMicroUSD = 0
			rec.usage.CostSource = "none"
			rec.provider, rec.model, rec.route = entry.Provider, entry.Model, entry.Route
			w.Header().Set("X-Spillway-Cache", "hit")
			w.Header().Set("X-Spillway-Provider", entry.Provider)
			w.Header().Set("X-Spillway-Model", entry.Model)
			w.Header().Set("X-Spillway-Route", entry.Route)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(entry.Body)
			return
		}
	}
	if virtual {
		h.servePlan(w, r, plan, body, cacheBypass)
		return
	}
	setRoute(w, r, "direct")
	ref, provider, ok := h.resolve(w, *body.Model)
	if !ok {
		return
	}
	if h.gate != nil {
		priceLookup, ok := h.pricer.(interface{ HasPrice(providers.ModelRef) bool })
		if !ok || !priceLookup.HasPrice(ref) {
			recordFrom(r.Context()).usage.ErrorKind = "model_price_unknown"
			writeError(w, http.StatusBadRequest, fmt.Sprintf("No price is configured for %s; add pricing.%s.", ref.String(), ref.String()), typeInvalidRequest, "model_price_unknown")
			return
		}
		if allowed, reset := h.gate.Allow(ref); !allowed {
			recordFrom(r.Context()).usage.ErrorKind = "budget_exceeded"
			setBudgetRetry(w, reset, h.now())
			writeError(w, http.StatusTooManyRequests, budgetMessage(reset, h.now()), typeAPI, "insufficient_quota")
			return
		}
	}

	rec = recordFrom(r.Context())
	rec.setProvider(ref)
	// Set before calling the provider so they are present on errors too.
	w.Header().Set("X-Spillway-Provider", ref.Provider)
	w.Header().Set("X-Spillway-Model", ref.Model)

	req := providers.ChatRequest{Model: ref.Model, Messages: body.Messages, RawBody: body.RawBody}
	if body.Stream {
		rec.beginAttempt(ref)
		streamReq, stripUsage := streamUsageRequest(req, h.captureStreamUsage)
		committed, _, err := serveStream(w, r, ref, provider, streamReq, h.chunkWriteTimeout, h.captureStreamUsage, stripUsage)
		if err != nil {
			rec.setError(err)
			rec.endAttempt(candidateErrorKind(err))
		} else {
			rec.usage.ErrorKind = ""
			rec.endAttempt("success")
		}
		if err != nil && !committed {
			writeProviderError(w, r, ref, err)
		}
		return
	}
	rec.beginAttempt(ref)
	resp, err := provider.Chat(r.Context(), req)
	if err != nil {
		recordResponseUsage(resp.Body, rec)
		rec.setError(err)
		rec.endAttempt(candidateErrorKind(err))
		writeProviderError(w, r, ref, err)
		return
	}
	recordResponseUsage(resp.Body, rec)
	rec.usage.ErrorKind = ""
	rec.endAttempt("success")
	h.storeCacheEntry(rec, body, resp.Body, cacheBypass)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp.Body)
}

func (h *chatHandler) writePlanError(w http.ResponseWriter, model string, err error) {
	switch {
	case errors.Is(err, router.ErrUnknownTier), errors.Is(err, router.ErrAutoNotConfigured):
		writeError(w, http.StatusNotFound, err.Error(), typeInvalidRequest, "model_not_found")
	case errors.Is(err, router.ErrInvalidField):
		writeError(w, http.StatusBadRequest, err.Error(), typeInvalidRequest, "invalid_request")
	case errors.Is(err, router.ErrNoCapableModel):
		code := "no_capable_model"
		var noCapable *router.NoCapableModelError
		if errors.As(err, &noCapable) && noCapable.EverySkipWasSize {
			code = "context_length_exceeded"
		}
		writeError(w, http.StatusBadRequest, err.Error(), typeInvalidRequest, code)
	default:
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("Could not plan model %q.", model), typeAPI, "internal_error")
	}
}

type candidateFailure struct {
	ref    providers.ModelRef
	reason string
	until  time.Time
}

func (h *chatHandler) servePlan(w http.ResponseWriter, r *http.Request, plan router.Plan, body chatBody, cacheBypass bool) {
	tier := plan.Source
	if plan.Source == "tier" && len(plan.Tiers) > 0 {
		tier = plan.Tiers[0]
	}
	candidates := plan.Eligible
	rec := recordFrom(r.Context())
	rec.beginTier(tier)
	rec.addPlanSkips(plan.Skipped)
	failures := make([]candidateFailure, 0, len(plan.Skipped)+len(candidates))
	for _, skip := range plan.Skipped {
		failures = append(failures, candidateFailure{ref: skip.Ref, reason: skip.Reason})
	}
	eligibleFailures := make([]candidateFailure, 0, len(candidates))
	for i, ref := range candidates {
		candidateTier := tier
		if i < len(plan.Tiers) {
			candidateTier = plan.Tiers[i]
		}
		if r.Context().Err() != nil {
			return
		}
		rec.err = nil
		if h.tracker != nil {
			if available, until, reason := h.tracker.Available(ref); !available {
				failure := candidateFailure{ref: ref, reason: cooldownReason(reason, until, h.now()), until: until}
				rec.addSkip(ref, fixedSkipReason(reason))
				failures = append(failures, failure)
				eligibleFailures = append(eligibleFailures, failure)
				continue
			}
		}
		if h.gate != nil {
			if allowed, reset := h.gate.Allow(ref); !allowed {
				reason := budgetReason(reset, h.now())
				failure := candidateFailure{ref: ref, reason: reason, until: reset}
				rec.addSkip(ref, fixedSkipReason(reason))
				failures = append(failures, failure)
				eligibleFailures = append(eligibleFailures, failure)
				continue
			}
		}
		provider, ok := h.providers.Lookup(ref.Provider)
		if !ok {
			// A validated config cannot reach this path, but an incomplete API option should fail safely.
			err := providers.ErrModelNotFound
			reason := candidateReason(err)
			failure := candidateFailure{ref: ref, reason: reason}
			rec.addSkip(ref, "unavailable")
			failures = append(failures, failure)
			eligibleFailures = append(eligibleFailures, failure)
			h.logCandidateFailure(r, candidateTier, ref, candidateErrorKind(err))
			continue
		}
		clearCandidateHeaders(w)
		route := candidateRoute(plan, candidateTier, i+1, len(candidates))
		setRoute(w, r, route)
		req := providers.ChatRequest{Model: ref.Model, Messages: body.Messages, RawBody: body.RawBody}
		rec.beginAttempt(ref)
		var err error
		if body.Stream {
			streamReq, stripUsage := streamUsageRequest(req, h.captureStreamUsage)
			rec.setProvider(ref)
			w.Header().Set("X-Spillway-Provider", ref.Provider)
			w.Header().Set("X-Spillway-Model", ref.Model)
			var committed bool
			var hint providers.RateLimit
			committed, hint, err = serveStream(w, r, ref, provider, streamReq, h.chunkWriteTimeout, h.captureStreamUsage, stripUsage)
			if committed {
				if err != nil {
					rec.setError(err)
					rec.endAttempt(candidateErrorKind(err))
				} else {
					rec.usage.ErrorKind = ""
					rec.endAttempt("success")
				}
				if h.tracker != nil && r.Context().Err() == nil {
					h.tracker.Observe(ref, err, hint)
				}
				return
			}
		} else {
			resp, chatErr := provider.Chat(r.Context(), req)
			if chatErr == nil {
				recordResponseUsage(resp.Body, rec)
				rec.usage.ErrorKind = ""
				rec.endAttempt("success")
				if h.tracker != nil {
					h.tracker.Observe(ref, nil, resp.RateLimit)
				}
				rec.setProvider(ref)
				w.Header().Set("X-Spillway-Provider", ref.Provider)
				w.Header().Set("X-Spillway-Model", ref.Model)
				h.storeCacheEntry(rec, body, resp.Body, cacheBypass)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(resp.Body)
				return
			}
			recordResponseUsage(resp.Body, rec)
			err = chatErr
		}
		if err != nil {
			rec.setError(err)
			rec.endAttempt(candidateErrorKind(err))
		}
		if r.Context().Err() != nil {
			rec.usage.ErrorKind = "client_disconnected"
			return
		}
		if h.tracker != nil {
			h.tracker.Observe(ref, err, providers.RateLimit{})
			if available, until, reason := h.tracker.Available(ref); !available {
				failure := candidateFailure{ref: ref, reason: cooldownReason(reason, until, h.now()), until: until}
				failures = append(failures, failure)
				eligibleFailures = append(eligibleFailures, failure)
			} else {
				failure := candidateFailure{ref: ref, reason: candidateReason(err)}
				failures = append(failures, failure)
				eligibleFailures = append(eligibleFailures, failure)
			}
		} else {
			failure := candidateFailure{ref: ref, reason: candidateReason(err)}
			failures = append(failures, failure)
			eligibleFailures = append(eligibleFailures, failure)
		}
		h.logCandidateFailure(r, candidateTier, ref, candidateErrorKind(err))
		if !isSpillable(err) {
			rec.setError(err)
			writeProviderError(w, r, ref, err)
			return
		}
	}
	clearCandidateHeaders(w)
	setRoute(w, r, exhaustedRoute(plan, tier, len(candidates)))
	message := exhaustionMessage(tier, failures)
	rec.err = errors.New("no candidate available")
	if retryAfter, allCooling := earliestCooldown(eligibleFailures, len(candidates), h.now()); allCooling {
		if allBudgetBlocked(eligibleFailures) {
			rec.usage.ErrorKind = "budget_exceeded"
		} else {
			rec.usage.ErrorKind = "rate_limit_exceeded"
		}
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeError(w, http.StatusTooManyRequests, message, typeAPI, "rate_limit_exceeded")
		return
	}
	rec.usage.ErrorKind = "no_candidate_available"
	writeError(w, http.StatusServiceUnavailable, message, typeAPI, "no_candidate_available")
}

func bypassCache(r *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Spillway-Cache")), "bypass")
}

func (h *chatHandler) storeCacheEntry(rec *requestRecord, body chatBody, response []byte, bypass bool) {
	if h.exactCache == nil || body.Stream || bypass || len(response) == 0 || len(response) > 1<<20 {
		return
	}
	h.exactCache.Set(rec.clientApp, body.RawBody, cache.Entry{
		Body: response, Provider: rec.provider, Model: rec.model, Route: rec.route,
		PromptTokens: rec.usage.PromptTokens, CompletionTokens: rec.usage.CompletionTokens,
		UsageSource: rec.usage.UsageSource,
	})
}

func allBudgetBlocked(failures []candidateFailure) bool {
	if len(failures) == 0 {
		return false
	}
	for _, failure := range failures {
		if !strings.HasPrefix(failure.reason, "budget exceeded") {
			return false
		}
	}
	return true
}

func budgetReason(reset, now time.Time) string {
	return "budget exceeded (resets in " + resetDuration(reset, now) + ")"
}

func budgetMessage(reset, now time.Time) string {
	return "Spend budget exceeded; paid models are available again in " + resetDuration(reset, now) + "."
}

func resetDuration(reset, now time.Time) string {
	remaining := reset.Sub(now)
	if remaining < 0 {
		remaining = 0
	}
	minutes := int((remaining + time.Second*30) / time.Minute)
	if minutes < 1 {
		minutes = 1
	}
	if minutes < 60 {
		return fmt.Sprintf("%dm", minutes)
	}
	hours, leftoverMinutes := minutes/60, minutes%60
	if hours < 24 {
		if leftoverMinutes == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh%dm", hours, leftoverMinutes)
	}
	days, leftoverHours := hours/24, hours%24
	return fmt.Sprintf("%dd%dh", days, leftoverHours)
}

func setBudgetRetry(w http.ResponseWriter, reset, now time.Time) {
	remaining := reset.Sub(now)
	seconds := int((remaining + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
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
	w.Header().Del("X-Spillway-Route")
	w.Header().Del("Retry-After")
}

func setRoute(w http.ResponseWriter, r *http.Request, route string) {
	if len(route) > 256 {
		route = route[:256]
	}
	w.Header().Set("X-Spillway-Route", route)
	recordFrom(r.Context()).route = route
}

func candidateRoute(plan router.Plan, tier string, candidate, total int) string {
	var parts []string
	if plan.Source == "auto" {
		parts = append(parts, "auto")
	} else {
		parts = append(parts, "tier "+safeTierName(tier))
	}
	if needs := routeNeeds(plan.Needs); needs != "" {
		parts = append(parts, "needs "+needs)
	}
	if plan.Source == "auto" {
		parts = append(parts, "tier "+safeTierName(tier))
	}
	parts = append(parts, fmt.Sprintf("candidate %d/%d", candidate, total))
	return strings.Join(parts, "; ")
}

func planErrorRoute(model string, plan router.Plan) string {
	var parts []string
	if model == "auto" {
		parts = append(parts, "auto")
	} else if strings.HasPrefix(model, "spillway/") {
		parts = append(parts, "tier "+safeTierName(strings.TrimPrefix(model, "spillway/")))
	} else {
		parts = append(parts, "auto")
	}
	if needs := routeNeeds(plan.Needs); needs != "" {
		parts = append(parts, "needs "+needs)
	}
	parts = append(parts, "no capable model")
	return strings.Join(parts, "; ")
}

func exhaustedRoute(plan router.Plan, tier string, eligible int) string {
	var parts []string
	if plan.Source == "auto" {
		parts = append(parts, "auto")
	} else {
		parts = append(parts, "tier "+safeTierName(tier))
	}
	if needs := routeNeeds(plan.Needs); needs != "" {
		parts = append(parts, "needs "+needs)
	}
	parts = append(parts, fmt.Sprintf("exhausted (%d eligible)", eligible))
	return strings.Join(parts, "; ")
}

func routeNeeds(needs []router.Capability) string {
	parts := make([]string, len(needs))
	for i, need := range needs {
		parts[i] = string(need)
	}
	return strings.Join(parts, ", ")
}

func safeTierName(name string) string {
	if name == "" || len(name) > 64 {
		return "unknown"
	}
	for i := 0; i < len(name); i++ {
		if (name[i] < 'a' || name[i] > 'z') && (name[i] < '0' || name[i] > '9') && name[i] != '-' {
			return "unknown"
		}
	}
	return name
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
