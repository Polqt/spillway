package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/Polqt/spillway/internal/providers"
)

// serveStream relays a provider stream as server-sent events, one flushed
// "data:" event per chunk, ending with "data: [DONE]". Headers are sent with
// the first chunk, so a failure before it is still a plain HTTP error.
func serveStream(w http.ResponseWriter, r *http.Request, ref providers.ModelRef, provider providers.Provider, req providers.ChatRequest) {
	rec := recordFrom(r.Context())
	rec.stream = true
	stream, err := provider.ChatStream(r.Context(), req)
	if err != nil {
		rec.err = err
		writeProviderError(w, r, ref, err)
		return
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
				writeProviderError(w, r, ref, err)
				return
			}
			if r.Context().Err() == nil {
				writeStreamError(w, rc, ref, err)
			}
			return
		}
		if !started {
			startEventStream(w)
			started = true
		}
		if errors.Is(err, io.EOF) {
			if err := writeEvent(w, rc, []byte("[DONE]")); err != nil {
				rec.err = err
			}
			return
		}
		if err := writeEvent(w, rc, chunk.Data); err != nil {
			rec.err = err
			return
		}
	}
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
func writeStreamError(w http.ResponseWriter, rc *http.ResponseController, ref providers.ModelRef, err error) {
	_, detail := providerError(ref, err)
	// The stream ends here either way; a failed write has nothing left to stop.
	_ = writeEvent(w, rc, errorJSON(detail))
}

func writeEvent(w http.ResponseWriter, rc *http.ResponseController, data []byte) error {
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return fmt.Errorf("write event to client: %w", err)
	}
	if err := rc.Flush(); err != nil {
		return fmt.Errorf("flush event to client: %w", err)
	}
	return nil
}
