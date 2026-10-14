package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Polqt/spillway/internal/providers"
)

// maxBodyBytes bounds a chat request; 10 MiB fits several base64 images.
const maxBodyBytes = 10 << 20

type chatHandler struct {
	providers providerSet
}

// chatBody is the part of a chat request Spillway acts on. Model is a
// pointer so a missing model is told apart from an empty one.
type chatBody struct {
	Model    *string             `json:"model"`
	Messages []providers.Message `json:"messages"`
	Stream   bool                `json:"stream"`
}

func (h *chatHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeChat(w, r)
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

	req := providers.ChatRequest{Model: ref.Model, Messages: body.Messages}
	if body.Stream {
		serveStream(w, r, ref, provider, req)
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

func decodeChat(w http.ResponseWriter, r *http.Request) (chatBody, bool) {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	var body chatBody
	err := dec.Decode(&body)
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
	writeErrorDetail(w, status, detail)
}

// providerError maps a provider error to an HTTP status and the OpenAI error
// shape. The client never sees the provider's error text; the access log
// records it.
func providerError(ref providers.ModelRef, err error) (int, errorDetail) {
	switch {
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
	default:
		return http.StatusInternalServerError, internalError()
	}
}
