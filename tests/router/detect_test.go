package router_test

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/Polqt/spillway/internal/router"
)

func TestDetect_Requirements(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want string
	}{
		{"plain text", `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`, ""},
		{"tools", `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}]}`, "tools"},
		{"legacy functions", `{"messages":[{"role":"user","content":"hi"}],"functions":[{"name":"f"}]}`, "tools"},
		{"tool message without tools", `{"messages":[{"role":"user","content":"hi"},{"role":"tool","tool_call_id":"1","content":"42"}]}`, "tools"},
		{"function message", `{"messages":[{"role":"function","name":"f","content":"42"}]}`, "tools"},
		{"assistant tool calls", `{"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"1","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`, "tools"},
		{"assistant function call", `{"messages":[{"role":"assistant","content":null,"function_call":{"name":"f","arguments":"{}"}}]}`, "tools"},
		{"image part", `{"messages":[{"role":"user","content":[{"type":"text","text":"what"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`, "vision"},
		{"json mode", `{"messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`, "json_mode"},
		{"structured outputs", `{"messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"name":"x","schema":{}}}}`, "structured_outputs"},
		{"all in fixed order", `{"response_format":{"type":"json_object"},"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"u"}}]}],"tools":[{"type":"function"}]}`, "tools,vision,json_mode"},
		{"empty tools", `{"messages":[{"role":"user","content":"hi"}],"tools":[],"functions":[]}`, ""},
		{"null tools", `{"messages":[{"role":"user","content":"hi"}],"tools":null,"response_format":null}`, ""},
		{"empty tool calls", `{"messages":[{"role":"assistant","content":"x","tool_calls":[]}]}`, ""},
		{"text response format", `{"messages":[{"role":"user","content":"hi"}],"response_format":{"type":"text"}}`, ""},
		{"unknown response format", `{"messages":[{"role":"user","content":"hi"}],"response_format":{"type":"future"}}`, ""},
		{"unknown content part", `{"messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"x"}}]}]}`, ""},
		{"null content", `{"messages":[{"role":"assistant","content":null}]}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := router.Detect([]byte(tt.body))
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			names := make([]string, len(got.Needs))
			for i, c := range got.Needs {
				names[i] = string(c)
			}
			if strings.Join(names, ",") != tt.want {
				t.Errorf("Needs = %v, want %q", names, tt.want)
			}
		})
	}
}

func TestDetect_SizeEstimate(t *testing.T) {
	t.Parallel()
	image := base64.StdEncoding.EncodeToString(make([]byte, 2<<20))
	tools := `[{"type":"function","function":{"name":"lookup"}}]`
	tests := []struct {
		name string
		body string
		want int
	}{
		{"string content", `{"messages":[{"role":"user","content":"` + strings.Repeat("a", 600) + `"}]}`, 100},
		{"rounds up", `{"messages":[{"role":"user","content":"` + strings.Repeat("a", 601) + `"}]}`, 101},
		{"text parts", `{"messages":[{"role":"user","content":[{"type":"text","text":"` + strings.Repeat("a", 300) + `"},{"type":"text","text":"` + strings.Repeat("b", 300) + `"}]}]}`, 100},
		{"image excluded", `{"messages":[{"role":"user","content":[{"type":"text","text":"` + strings.Repeat("a", 60) + `"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + image + `"}}]}]}`, 10},
		{"tool definitions counted", `{"messages":[{"role":"user","content":""}],"tools":` + tools + `}`, (len(tools) + 5) / 6},
		{"max tokens added", `{"messages":[{"role":"user","content":"` + strings.Repeat("a", 6) + `"}],"max_tokens":100}`, 101},
		{"max completion tokens wins", `{"messages":[{"role":"user","content":"` + strings.Repeat("a", 6) + `"}],"max_tokens":100,"max_completion_tokens":50}`, 51},
		{"null max tokens", `{"messages":[{"role":"user","content":"abcdef"}],"max_tokens":null}`, 1},
		{"huge max tokens saturates", `{"messages":[{"role":"user","content":"abcdef"}],"max_tokens":9223372036854775807}`, 1<<31 - 1 + 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := router.Detect([]byte(tt.body))
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if got.EstimatedTokens != tt.want {
				t.Errorf("EstimatedTokens = %d, want %d", got.EstimatedTokens, tt.want)
			}
		})
	}
}

func TestDetect_WrongFieldTypes(t *testing.T) {
	t.Parallel()
	tests := []struct{ body, field string }{
		{`{"messages":[{"role":"user","content":"hi"}],"tools":"x"}`, "tools"},
		{`{"messages":[{"role":"user","content":"hi"}],"functions":{}}`, "functions"},
		{`{"messages":[{"role":"user","content":"hi"}],"response_format":3}`, "response_format"},
		{`{"messages":[{"role":"user","content":"hi"}],"response_format":{"type":7}}`, "response_format"},
		{`{"messages":[{"role":"user","content":"hi"}],"max_tokens":"many"}`, "max_tokens"},
		{`{"messages":[{"role":"user","content":"hi"}],"max_completion_tokens":1.5}`, "max_completion_tokens"},
		{`{"messages":[{"role":"user","content":7}]}`, "messages[0].content"},
		{`{"messages":[{"role":"user","content":"a"},{"role":"user","content":[7]}]}`, "messages[1].content"},
		{`{"messages":[{"role":"user","content":[{"type":"text","text":5}]}]}`, "messages[0].content"},
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			t.Parallel()
			_, err := router.Detect([]byte(tt.body))
			if !errors.Is(err, router.ErrInvalidField) {
				t.Fatalf("Detect error = %v, want ErrInvalidField", err)
			}
			if !strings.Contains(err.Error(), `"`+tt.field+`"`) {
				t.Errorf("Detect error = %q, want it to name %q", err, tt.field)
			}
		})
	}
}
