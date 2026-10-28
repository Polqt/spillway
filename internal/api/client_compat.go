package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type clientProtocol uint8

const (
	protocolAnthropic clientProtocol = iota + 1
	protocolResponses
)

type clientCompatibilityHandler struct {
	chat     *chatHandler
	protocol clientProtocol
}

func (h *clientCompatibilityHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, err := readClientBody(w, r, h.chat.bodyReadTimeout)
	if err != nil {
		writeClientError(w, h.protocol, http.StatusBadRequest, err.Error(), "invalid_request")
		return
	}
	chatBody, stream, toolNamespaces, err := translateClientRequest(h.protocol, raw)
	if err != nil {
		writeClientError(w, h.protocol, http.StatusBadRequest, err.Error(), "invalid_request")
		return
	}
	request := r.Clone(r.Context())
	request.URL.Path = "/v1/chat/completions"
	request.Body = io.NopCloser(bytes.NewReader(chatBody))
	request.ContentLength = int64(len(chatBody))
	response := &clientCompatibilityWriter{
		ResponseWriter: w,
		protocol:       h.protocol,
		stream:         stream,
		toolNamespaces: toolNamespaces,
		logger:         h.chat.logger,
	}
	h.chat.ServeHTTP(response, request)
	response.finish()
}

func readClientBody(w http.ResponseWriter, r *http.Request, timeout time.Duration) ([]byte, error) {
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(timeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return nil, errors.New("request body deadline could not be set")
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if clearErr := rc.SetReadDeadline(time.Time{}); err == nil && clearErr != nil && !errors.Is(clearErr, http.ErrNotSupported) {
		err = clearErr
	}
	if err != nil {
		if maxErr := (*http.MaxBytesError)(nil); errors.As(err, &maxErr) {
			r.Close = true
			_ = r.Body.Close()
			return nil, errors.New("request body is larger than 10 MiB")
		}
		r.Close = true
		_ = r.Body.Close()
		return nil, errors.New("request body could not be read")
	}
	var value json.RawMessage
	if err := json.Unmarshal(data, &value); err != nil || len(value) == 0 || value[0] != '{' {
		return nil, errors.New("request body must be one JSON object")
	}
	return data, nil
}

func decodeClientJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request must contain one JSON value")
		}
		return err
	}
	return nil
}

func translateClientRequest(protocol clientProtocol, raw []byte) ([]byte, bool, map[string]responseToolNamespace, error) {
	switch protocol {
	case protocolAnthropic:
		body, stream, err := anthropicRequest(raw)
		return body, stream, nil, err
	case protocolResponses:
		return responsesRequest(raw)
	default:
		return nil, false, nil, errors.New("unsupported client protocol")
	}
}

type clientCompatibilityWriter struct {
	http.ResponseWriter
	protocol       clientProtocol
	stream         bool
	toolNamespaces map[string]responseToolNamespace
	logger         *slog.Logger
	status         int
	started        bool
	failed         bool
	body           bytes.Buffer
	pending        []byte
	response       responsesStreamState
	message        anthropicStreamState
}

func (w *clientCompatibilityWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *clientCompatibilityWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	if w.stream && status < http.StatusBadRequest {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.started = true
		w.ResponseWriter.WriteHeader(status)
		return
	}
}

func (w *clientCompatibilityWriter) Write(data []byte) (int, error) {
	if w.status >= http.StatusBadRequest || !w.stream {
		return w.body.Write(data)
	}
	w.pending = append(w.pending, data...)
	for {
		boundary := bytes.Index(w.pending, []byte("\n\n"))
		if boundary < 0 {
			break
		}
		frame := w.pending[:boundary]
		w.pending = w.pending[boundary+2:]
		w.writeChatFrame(frame)
	}
	return len(data), nil
}

func (w *clientCompatibilityWriter) finish() {
	if w.stream && w.status < http.StatusBadRequest && w.started {
		if len(w.pending) > 0 {
			w.writeChatFrame(w.pending)
			w.pending = nil
		}
		w.finishStream()
		return
	}
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	if status >= http.StatusBadRequest {
		w.Header().Set("Content-Type", "application/json")
		w.ResponseWriter.WriteHeader(status)
		_, _ = w.ResponseWriter.Write(clientErrorBody(w.protocol, w.body.Bytes()))
		return
	}
	body, err := translateClientResponse(w.protocol, w.body.Bytes(), w.toolNamespaces, w.logger, w.Header().Get("X-Spillway-Provider"))
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.ResponseWriter.WriteHeader(http.StatusBadGateway)
		_, _ = w.ResponseWriter.Write(clientErrorBody(w.protocol, errorJSON(internalError())))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.ResponseWriter.WriteHeader(status)
	_, _ = w.ResponseWriter.Write(body)
}

func (w *clientCompatibilityWriter) writeChatFrame(frame []byte) {
	for _, line := range bytes.Split(frame, []byte("\n")) {
		data := strings.TrimSpace(string(line))
		if !strings.HasPrefix(data, "data:") {
			continue
		}
		data = strings.TrimSpace(strings.TrimPrefix(data, "data:"))
		if data == "[DONE]" {
			w.finishStream()
			continue
		}
		var chunk map[string]json.RawMessage
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			w.writeStreamError("Provider returned an invalid stream event.")
			continue
		}
		if _, failed := chunk["error"]; failed {
			message := "Provider request failed."
			var detail struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(chunk["error"], &detail); err == nil && detail.Message != "" {
				message = detail.Message
			}
			w.writeStreamError(message)
			continue
		}
		if w.protocol == protocolResponses {
			w.response.writeChunk(w, chunk)
		} else {
			w.message.writeChunk(w, chunk)
		}
	}
}

func (w *clientCompatibilityWriter) finishStream() {
	if w.failed {
		return
	}
	if w.protocol == protocolResponses {
		w.response.finish(w)
	} else {
		w.message.finish(w, "end_turn")
	}
}

func (w *clientCompatibilityWriter) writeStreamError(message string) {
	w.failed = true
	if w.protocol == protocolResponses {
		w.writeEvent("error", map[string]any{"type": "error", "code": "upstream_error", "message": message, "param": nil})
	} else {
		w.writeEvent("error", map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": message}})
	}
}

func (w *clientCompatibilityWriter) writeEvent(name string, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w.ResponseWriter, "event: %s\ndata: %s\n\n", name, data)
}

func writeClientError(w http.ResponseWriter, protocol clientProtocol, status int, message, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	errorType := typeInvalidRequest
	if code == "invalid_api_key" {
		errorType = typeAuthentication
	}
	_, _ = w.Write(clientErrorBody(protocol, errorJSON(errorDetail{Message: message, Type: errorType, Code: code})))
}

func clientErrorBody(protocol clientProtocol, raw []byte) []byte {
	if protocol != protocolAnthropic {
		return raw
	}
	var source struct {
		Error errorDetail `json:"error"`
	}
	if err := json.Unmarshal(raw, &source); err != nil {
		return []byte(`{"type":"error","error":{"type":"api_error","message":"Request failed."}}`)
	}
	typ := source.Error.Type
	switch typ {
	case typeInvalidRequest:
		typ = "invalid_request_error"
	case typeAuthentication:
		typ = "authentication_error"
	case typeAPI:
		typ = "api_error"
	}
	encoded, err := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": source.Error.Message}})
	if err != nil {
		return []byte(`{"type":"error","error":{"type":"api_error","message":"Request failed."}}`)
	}
	return encoded
}

func translateClientResponse(protocol clientProtocol, raw []byte, namespaces map[string]responseToolNamespace, logger *slog.Logger, provider string) ([]byte, error) {
	switch protocol {
	case protocolAnthropic:
		return anthropicResponse(raw, logger, provider)
	case protocolResponses:
		return responsesResponse(raw, namespaces, logger, provider)
	default:
		return nil, errors.New("unsupported client protocol")
	}
}
