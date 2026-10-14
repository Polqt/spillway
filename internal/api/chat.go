package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Polqt/spillway/internal/providers"
)

// maxBodyBytes bounds a chat request; 10 MiB fits several base64 images.
const maxBodyBytes = 10 << 20

type chatHandler struct {
	providers         providerSet
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
		serveStream(w, r, ref, provider, req, h.chunkWriteTimeout)
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
	if model == "auto" || strings.HasPrefix(model, "spillway/") {
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
