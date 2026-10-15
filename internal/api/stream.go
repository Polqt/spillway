package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Polqt/spillway/internal/providers"
)

// serveStream relays a provider stream as server-sent events, one flushed
// "data:" event per chunk, ending with "data: [DONE]". Headers are sent with
// the first chunk, so a failure before it is still a plain HTTP error.
// serveStream marks whether response bytes were committed, so callers can
// spill before the first chunk and still observe upstream failures afterward.
func serveStream(w http.ResponseWriter, r *http.Request, ref providers.ModelRef, provider providers.Provider, req providers.ChatRequest, writeTimeout time.Duration) (err error, committed bool) {
	rec := recordFrom(r.Context())
	rec.stream = true
	stream, err := provider.ChatStream(r.Context(), req)
	if err != nil {
		rec.err = err
		return err, false
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
				return err, false
			}
			if r.Context().Err() == nil {
				writeStreamError(w, rc, ref, err, writeTimeout)
			}
			return err, true
		}
		if !started {
			startEventStream(w)
			started = true
		}
		if errors.Is(err, io.EOF) {
			if err := writeEvent(w, rc, []byte("[DONE]"), writeTimeout); err != nil {
				rec.err = err
			}
			return nil, true
		}
		if err := writeEvent(w, rc, chunk.Data, writeTimeout); err != nil {
			rec.err = err
			return nil, true
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
