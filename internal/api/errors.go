package api

import (
	"bytes"
	"encoding/json"
	"net/http"
)

// OpenAI error types.
const (
	typeInvalidRequest = "invalid_request_error"
	typeAuthentication = "authentication_error"
	typeAPI            = "api_error"
)

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

// writeError writes the OpenAI error shape. message must never contain a
// secret, a presented token or a presented Host; echoing the client's own
// model reference, JSON-escaped, is fine.
func writeError(w http.ResponseWriter, status int, message, typ, code string) {
	writeErrorDetail(w, status, errorDetail{Message: message, Type: typ, Code: code})
}

func writeErrorDetail(w http.ResponseWriter, status int, detail errorDetail) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(errorJSON(detail), '\n'))
}

// errorJSON encodes detail in the OpenAI error shape.
func errorJSON(detail errorDetail) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// Keep "<key>" in messages readable instead of HTML-escaping its angle brackets.
	enc.SetEscapeHTML(false)
	// Encoding a struct of strings cannot fail.
	_ = enc.Encode(errorBody{Error: detail})
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

func writeInternalError(w http.ResponseWriter) {
	writeErrorDetail(w, http.StatusInternalServerError, internalError())
}

func internalError() errorDetail {
	return errorDetail{Message: "Internal error in Spillway; see its log for details.", Type: typeAPI, Code: "internal_error"}
}
