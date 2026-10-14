package api

import (
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
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	// Keep "<key>" in messages readable instead of HTML-escaping its angle brackets.
	enc.SetEscapeHTML(false)
	_ = enc.Encode(errorBody{Error: errorDetail{Message: message, Type: typ, Code: code}})
}

func writeInternalError(w http.ResponseWriter) {
	writeError(w, http.StatusInternalServerError, "Internal error in Spillway; see its log for details.", typeAPI, "internal_error")
}
