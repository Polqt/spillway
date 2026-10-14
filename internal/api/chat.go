package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

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

	resp, err := provider.Chat(r.Context(), providers.ChatRequest{Model: ref.Model, Messages: body.Messages})
	if err != nil {
		rec.err = err
		writeProviderError(w, ref, err)
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
	ref, err := providers.ParseModelRef(model)
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Model %q not found. Name a model as provider/model, such as mock/echo.", model), typeInvalidRequest, "model_not_found")
		return providers.ModelRef{}, nil, false
	}
	p, ok := h.providers.Lookup(ref.Provider)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Model %q not found: provider %q is not configured.", model, ref.Provider), typeInvalidRequest, "model_not_found")
		return providers.ModelRef{}, nil, false
	}
	return ref, p, true
}

// writeProviderError maps a provider error to the OpenAI shape. The client
// never sees the provider's error text; the access log records it.
func writeProviderError(w http.ResponseWriter, ref providers.ModelRef, err error) {
	if errors.Is(err, providers.ErrModelNotFound) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Model %q not found on provider %q.", ref.String(), ref.Provider), typeInvalidRequest, "model_not_found")
		return
	}
	writeInternalError(w)
}
