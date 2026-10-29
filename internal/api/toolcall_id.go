package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

const (
	signedToolCallIDPrefix   = "spwts1_"
	maxThoughtSignatureBytes = 64 << 10
	maxProviderToolCallID    = 256
	maxSignedEnvelopeBytes   = maxThoughtSignatureBytes + 2*maxProviderToolCallID + 32
	maxSignedToolCallIDBytes = len(signedToolCallIDPrefix) + (maxSignedEnvelopeBytes*8+5)/6
)

type toolCallRef struct {
	ID               string
	ThoughtSignature string
}

type googleExtraContent struct {
	Google struct {
		ThoughtSignature thoughtSignature `json:"thought_signature"`
	} `json:"google"`
}

// thoughtSignature decodes a non-string upstream value as no signature, so one odd field never fails a whole reply.
type thoughtSignature string

func (s *thoughtSignature) UnmarshalJSON(data []byte) error {
	var value string
	if json.Unmarshal(data, &value) != nil {
		value = ""
	}
	*s = thoughtSignature(value)
	return nil
}

func clientToolCallID(ref toolCallRef) (string, bool) {
	if ref.ThoughtSignature == "" || !validProviderToolCallID(ref.ID) || !validThoughtSignature(ref.ThoughtSignature) {
		return ref.ID, false
	}
	// HTML escaping would grow <, > and & sixfold, past the 2x that maxSignedEnvelopeBytes allows for the ID.
	var envelope bytes.Buffer
	encoder := json.NewEncoder(&envelope)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(struct {
		ID string `json:"id"`
		TS string `json:"ts"`
	}{ID: ref.ID, TS: ref.ThoughtSignature}); err != nil {
		return ref.ID, false
	}
	return signedToolCallIDPrefix + base64.RawURLEncoding.EncodeToString(bytes.TrimSuffix(envelope.Bytes(), []byte("\n"))), true
}

func parseClientToolCallID(id string) (toolCallRef, error) {
	if len(id) < len(signedToolCallIDPrefix) || id[:len(signedToolCallIDPrefix)] != signedToolCallIDPrefix {
		return toolCallRef{ID: id}, nil
	}
	if len(id) > maxSignedToolCallIDBytes {
		return toolCallRef{}, errors.New("signed tool-call ID is too large")
	}
	encoded := id[len(signedToolCallIDPrefix):]
	if strings.ContainsAny(encoded, "\r\n") {
		return toolCallRef{}, errors.New("signed tool-call ID is invalid")
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return toolCallRef{}, errors.New("signed tool-call ID is invalid")
	}
	var envelope struct {
		ID string `json:"id"`
		TS string `json:"ts"`
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return toolCallRef{}, errors.New("signed tool-call ID is invalid")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return toolCallRef{}, errors.New("signed tool-call ID is invalid")
	}
	if !validProviderToolCallID(envelope.ID) || !validThoughtSignature(envelope.TS) {
		return toolCallRef{}, errors.New("signed tool-call ID is invalid")
	}
	return toolCallRef{ID: envelope.ID, ThoughtSignature: envelope.TS}, nil
}

func chatToolCall(ref toolCallRef, name, arguments string) map[string]any {
	call := map[string]any{"id": ref.ID, "type": "function", "function": map[string]any{"name": name, "arguments": arguments}}
	if ref.ThoughtSignature != "" {
		var extra googleExtraContent
		extra.Google.ThoughtSignature = thoughtSignature(ref.ThoughtSignature)
		call["extra_content"] = extra
	}
	return call
}

func validProviderToolCallID(id string) bool {
	if len(id) == 0 || len(id) > maxProviderToolCallID {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

func validThoughtSignature(signature string) bool {
	if len(signature) == 0 || len(signature) > maxThoughtSignatureBytes {
		return false
	}
	for i := 0; i < len(signature); i++ {
		char := signature[i]
		switch {
		case char >= 'A' && char <= 'Z', char >= 'a' && char <= 'z', char >= '0' && char <= '9':
		case char == '+', char == '/', char == '=', char == '_', char == '-':
		default:
			return false
		}
	}
	return true
}

func signatureIssue(ref toolCallRef) string {
	if ref.ThoughtSignature == "" {
		return ""
	}
	if len(ref.ThoughtSignature) > maxThoughtSignatureBytes {
		return "too_large"
	}
	if !validProviderToolCallID(ref.ID) || !validThoughtSignature(ref.ThoughtSignature) {
		return "invalid"
	}
	return ""
}
