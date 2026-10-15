package api_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Polqt/spillway/internal/api"
	"github.com/Polqt/spillway/internal/fakenet"
	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/providers/mock"
	"github.com/Polqt/spillway/internal/providers/openaicompat"
	"github.com/Polqt/spillway/internal/quota"
	"github.com/Polqt/spillway/internal/redact"
)

const (
	keyCI     = "sk-test-local-ci"
	keyCursor = "sk-test-local-cursor"
	echoBody  = `{"model":"mock/echo","messages":[{"role":"user","content":"hello spillway"}]}`
)

// countingProvider records calls so tests can prove a request never reached a provider.
type countingProvider struct {
	name   string
	calls  atomic.Int32
	models func(context.Context) ([]providers.Model, error)
	chat   func(providers.ChatRequest) (providers.ChatResponse, error)
	stream func(context.Context, providers.ChatRequest) (providers.Stream, error)
}

type cancelingProvider struct {
	started chan struct{}
}

func (*cancelingProvider) Name() string { return "cancel" }
func (*cancelingProvider) Models(context.Context) ([]providers.Model, error) {
	return nil, nil
}
func (p *cancelingProvider) Chat(ctx context.Context, _ providers.ChatRequest) (providers.ChatResponse, error) {
	close(p.started)
	<-ctx.Done()
	return providers.ChatResponse{}, ctx.Err()
}
func (*cancelingProvider) ChatStream(context.Context, providers.ChatRequest) (providers.Stream, error) {
	return nil, errors.New("not used")
}

func (p *countingProvider) Name() string { return p.name }
func (p *countingProvider) Models(ctx context.Context) ([]providers.Model, error) {
	if p.models == nil {
		return nil, errors.New("models not configured")
	}
	return p.models(ctx)
}
func (p *countingProvider) Chat(_ context.Context, req providers.ChatRequest) (providers.ChatResponse, error) {
	p.calls.Add(1)
	return p.chat(req)
}
func (p *countingProvider) ChatStream(ctx context.Context, req providers.ChatRequest) (providers.Stream, error) {
	p.calls.Add(1)
	return p.stream(ctx, req)
}

// syncBuffer is a log sink safe for concurrent handler writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type env struct {
	handler http.Handler
	logs    *syncBuffer
	fake    *countingProvider
}

// newEnv builds a handler over the mock provider and a counting fake, with two
// local keys. hosts is passed through as Options.AllowedHosts.
func newEnv(t *testing.T, hosts []string) env {
	return newEnvWithTimeouts(t, hosts, 0, 0)
}

func newEnvWithTimeouts(t *testing.T, hosts []string, bodyReadTimeout, chunkWriteTimeout time.Duration) env {
	t.Helper()
	m, err := mock.New(mock.Options{Now: func() time.Time { return time.Unix(1760400000, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	fake := &countingProvider{name: "fake", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
		return providers.ChatResponse{Body: json.RawMessage(`{"object":"chat.completion"}`)}, nil
	}, stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) {
		return nil, errors.New("fake: no stream configured")
	}}
	set, err := providers.NewSet(m, fake)
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	h, err := api.NewHandler(api.Options{
		Providers: set,
		LocalKeys: []api.LocalKey{
			{Name: "ci", Key: redact.NewSecret(keyCI)},
			{Name: "cursor", Key: redact.NewSecret(keyCursor)},
		},
		AllowedHosts:    hosts,
		Logger:          slog.New(slog.NewTextHandler(logs, nil)),
		BodyReadTimeout: bodyReadTimeout, ChunkWriteTimeout: chunkWriteTimeout,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return env{handler: h, logs: logs, fake: fake}
}

func TestChat_BodyReadTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnvWithTimeouts(t, api.LoopbackHosts("127.0.0.1"), time.Second, time.Second)
		listener := fakenet.Listen()
		server := api.NewServer(e.handler, slog.New(slog.DiscardHandler))
		serveDone := make(chan error, 1)
		go func() { serveDone <- server.Serve(listener) }()
		t.Cleanup(func() { _ = server.Close(); _ = listener.Close() })

		conn, err := listener.DialContext(t.Context(), "", "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		request := "POST /v1/chat/completions HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer " + keyCI + "\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n"
		if _, err := conn.Write([]byte(request)); err != nil {
			t.Fatal(err)
		}
		type responseResult struct {
			status int
			body   []byte
			err    error
		}
		responseDone := make(chan responseResult, 1)
		go func() {
			response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
			if err != nil {
				responseDone <- responseResult{err: err}
				return
			}
			defer func() { _ = response.Body.Close() }()
			body, err := io.ReadAll(response.Body)
			responseDone <- responseResult{status: response.StatusCode, body: body, err: err}
		}()
		<-time.After(2 * time.Second)
		result := <-responseDone
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.status != http.StatusBadRequest || !strings.Contains(string(result.body), `"type":"invalid_request_error"`) {
			t.Fatalf("response = %d %s; want OpenAI 400", result.status, result.body)
		}
		if got := e.fake.calls.Load(); got != 0 {
			t.Errorf("provider calls = %d, want zero", got)
		}
		_ = server.Close()
		_ = listener.Close()
		if err := <-serveDone; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve() = %v, want http.ErrServerClosed", err)
		}
	})
}

type request struct {
	method, path, host, auth, body string
}

func (e env) do(t *testing.T, r request) *httptest.ResponseRecorder {
	t.Helper()
	if r.method == "" {
		r.method = http.MethodPost
	}
	if r.path == "" {
		r.path = "/v1/chat/completions"
	}
	if r.host == "" {
		r.host = "127.0.0.1:8080"
	}
	req := httptest.NewRequestWithContext(t.Context(), r.method, r.path, strings.NewReader(r.body))
	req.Host = r.host
	if r.auth != "" {
		req.Header.Set("Authorization", r.auth)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

type openAIError struct {
	Error struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Param   *string `json:"param"`
		Code    string  `json:"code"`
	} `json:"error"`
}

func assertError(t *testing.T, rec *httptest.ResponseRecorder, status int, typ, code string) {
	t.Helper()
	if rec.Code != status {
		t.Errorf("status = %d, want %d; body %s", rec.Code, status, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var e openAIError
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("body is not an OpenAI error: %v\n%s", err, rec.Body)
	}
	if e.Error.Type != typ || e.Error.Code != code || e.Error.Message == "" {
		t.Errorf("error = %+v, want type %s code %s and a message", e.Error, typ, code)
	}
	if !strings.Contains(rec.Body.String(), `"param":null`) {
		t.Errorf("body %s, want \"param\":null", rec.Body)
	}
}

func TestNewHandler_RequiresLocalKey(t *testing.T) {
	t.Parallel()
	set, err := providers.NewSet(&countingProvider{name: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	tests := map[string][]api.LocalKey{
		"nil":      nil,
		"empty":    {},
		"zero key": {{Name: "ci"}},
	}
	for name, keys := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, err := api.NewHandler(api.Options{Providers: set, LocalKeys: keys, Logger: logger})
			if err == nil || h != nil {
				t.Errorf("NewHandler = %v, %v; want nil handler and an error", h, err)
			}
		})
	}
}

func TestHealthz_OK(t *testing.T) {
	t.Parallel()
	e := newEnv(t, api.LoopbackHosts("127.0.0.1"))

	rec := e.do(t, request{method: http.MethodGet, path: "/healthz"})

	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("got %d %q, want 200 ok", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
}

// Security property: /healthz is exempt from auth only, not from the Host check.
func TestHealthz_SubjectToHostCheck(t *testing.T) {
	t.Parallel()
	e := newEnv(t, api.LoopbackHosts("127.0.0.1"))

	rec := e.do(t, request{method: http.MethodGet, path: "/healthz", host: "evil.example"})

	assertError(t, rec, http.StatusForbidden, "invalid_request_error", "host_not_allowed")
}

// Security property: only the loopback names and the listen host are accepted.
func TestHostCheck_Loopback(t *testing.T) {
	t.Parallel()
	e := newEnv(t, api.LoopbackHosts("127.0.0.1"))
	allowed := []string{"localhost", "localhost:8080", "LOCALHOST:8080", "127.0.0.1", "127.0.0.1:9999", "[::1]", "[::1]:8080"}
	refused := []string{"evil.example", "evil.example:8080", "localhost.evil.example", "127.0.0.1.nip.io", "localhost.", "0.0.0.0:8080", "[::1", " "}
	for _, host := range allowed {
		t.Run("allow "+host, func(t *testing.T) {
			t.Parallel()
			rec := e.do(t, request{host: host, auth: "Bearer " + keyCI, body: echoBody})
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200; body %s", rec.Code, rec.Body)
			}
		})
	}
	for _, host := range refused {
		t.Run("refuse "+host, func(t *testing.T) {
			t.Parallel()
			rec := e.do(t, request{host: host, auth: "Bearer " + keyCI, body: echoBody})
			assertError(t, rec, http.StatusForbidden, "invalid_request_error", "host_not_allowed")
		})
	}
}

func TestHostCheck_EmptyHostRefused(t *testing.T) {
	t.Parallel()
	e := newEnv(t, api.LoopbackHosts("127.0.0.1"))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)
	req.Host = ""
	rec := httptest.NewRecorder()

	e.handler.ServeHTTP(rec, req)

	assertError(t, rec, http.StatusForbidden, "invalid_request_error", "host_not_allowed")
}

func TestHostCheck_IncludesListenHost(t *testing.T) {
	t.Parallel()
	e := newEnv(t, api.LoopbackHosts("127.0.0.5"))

	if rec := e.do(t, request{host: "127.0.0.5:8080", auth: "Bearer " + keyCI, body: echoBody}); rec.Code != http.StatusOK {
		t.Errorf("listen host: status = %d, want 200", rec.Code)
	}
	if rec := e.do(t, request{host: "127.0.0.6:8080", auth: "Bearer " + keyCI, body: echoBody}); rec.Code != http.StatusForbidden {
		t.Errorf("other loopback IP: status = %d, want 403", rec.Code)
	}
}

func TestLoopbackHosts_Deduplicates(t *testing.T) {
	t.Parallel()
	got := api.LoopbackHosts("LocalHost")
	want := []string{"localhost", "127.0.0.1", "::1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("LoopbackHosts(LocalHost) = %v, want %v", got, want)
	}
}

// Security property: a rebound request never reaches auth or a provider.
func TestHostCheck_RunsBeforeAuth(t *testing.T) {
	t.Parallel()
	e := newEnv(t, api.LoopbackHosts("127.0.0.1"))
	body := `{"model":"fake/x","messages":[{"role":"user","content":"hi"}]}`

	for _, auth := range []string{"Bearer " + keyCI, ""} {
		rec := e.do(t, request{host: "evil.example", auth: auth, body: body})
		assertError(t, rec, http.StatusForbidden, "invalid_request_error", "host_not_allowed")
	}
	if n := e.fake.calls.Load(); n != 0 {
		t.Errorf("provider called %d times, want 0", n)
	}
	if strings.Contains(e.logs.String(), "client_app") {
		t.Errorf("log has client_app for refused requests:\n%s", e.logs)
	}
}

// Security property: the check applies only when enabled, and auth still applies.
func TestHostCheck_DisabledOffLoopback(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)

	if rec := e.do(t, request{host: "evil.example", auth: "Bearer " + keyCI, body: echoBody}); rec.Code != http.StatusOK {
		t.Errorf("with key: status = %d, want 200", rec.Code)
	}
	assertError(t, e.do(t, request{host: "evil.example", body: echoBody}), http.StatusUnauthorized, "authentication_error", "invalid_api_key")
}

func TestHostCheck_RefusalNeverEchoesHostOrKey(t *testing.T) {
	t.Parallel()
	e := newEnv(t, api.LoopbackHosts("127.0.0.1"))

	rec := e.do(t, request{host: "evil.example", auth: "Bearer " + keyCI, body: echoBody})

	for _, s := range []string{"evil.example", keyCI} {
		if strings.Contains(rec.Body.String(), s) {
			t.Errorf("body contains %q: %s", s, rec.Body)
		}
		if strings.Contains(e.logs.String(), s) {
			t.Errorf("log contains %q:\n%s", s, e.logs)
		}
	}
}

// Security property: every /v1/* path requires a local key, loopback included.
func TestAuth_Required(t *testing.T) {
	t.Parallel()
	e := newEnv(t, api.LoopbackHosts("127.0.0.1"))
	tests := []struct {
		name, auth string
		ok         bool
	}{
		{name: "no header", auth: ""},
		{name: "wrong scheme", auth: "Basic " + keyCI},
		{name: "wrong key", auth: "Bearer wrong-key"},
		{name: "surrounding whitespace", auth: "Bearer  " + keyCI + " "},
		{name: "prefix of a key", auth: "Bearer " + keyCI[:len(keyCI)-1]},
		{name: "key plus a character", auth: "Bearer " + keyCI + "x"},
		{name: "bare token", auth: keyCI},
		{name: "first key", auth: "Bearer " + keyCI, ok: true},
		{name: "second key", auth: "Bearer " + keyCursor, ok: true},
		{name: "lowercase scheme", auth: "bearer " + keyCI, ok: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := e.do(t, request{auth: tt.auth, body: echoBody})
			if tt.ok {
				if rec.Code != http.StatusOK {
					t.Errorf("status = %d, want 200; body %s", rec.Code, rec.Body)
				}
				return
			}
			assertError(t, rec, http.StatusUnauthorized, "authentication_error", "invalid_api_key")
			if !strings.Contains(rec.Body.String(), "Bearer <key>") {
				t.Errorf("body %s, want the readable hint \"Bearer <key>\"", rec.Body)
			}
		})
	}
}

func TestAuth_ResolvesClientApp(t *testing.T) {
	t.Parallel()
	tests := []struct {
		key, want string
	}{
		{keyCI, "client_app=ci"},
		{keyCursor, "client_app=cursor"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, nil)
			e.do(t, request{auth: "Bearer " + tt.key, body: echoBody})
			if !strings.Contains(e.logs.String(), tt.want) {
				t.Errorf("log lacks %s:\n%s", tt.want, e.logs)
			}
		})
	}

	e := newEnv(t, nil)
	e.do(t, request{auth: "Bearer wrong-key", body: echoBody})
	if strings.Contains(e.logs.String(), "client_app") {
		t.Errorf("failed request logged client_app:\n%s", e.logs)
	}
}

// Security property: logs carry the client app name, never a local key or a
// presented token, and failures never echo the token.
func TestAuth_KeyNeverLoggedOrEchoed(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	const presented = "sk-test-presented-wrong"
	var responses []string
	for _, auth := range []string{"Bearer " + keyCI, "Bearer " + keyCursor, "Bearer " + presented, presented} {
		rec := e.do(t, request{auth: auth, body: echoBody})
		responses = append(responses, rec.Body.String())
		for name, values := range rec.Header() {
			if strings.Contains(name+strings.Join(values, ","), presented) {
				t.Errorf("response header %s echoes the token", name)
			}
		}
	}
	for _, s := range []string{keyCI, keyCursor, presented} {
		if strings.Contains(e.logs.String(), s) {
			t.Errorf("log contains %q:\n%s", s, e.logs)
		}
		for _, body := range responses {
			if strings.Contains(body, s) {
				t.Errorf("response body contains %q: %s", s, body)
			}
		}
	}
}

func TestChat_EchoMock(t *testing.T) {
	t.Parallel()
	e := newEnv(t, api.LoopbackHosts("127.0.0.1"))

	rec := e.do(t, request{auth: "Bearer " + keyCI, body: echoBody})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	for header, want := range map[string]string{
		"Content-Type":        "application/json",
		"X-Spillway-Provider": "mock",
		"X-Spillway-Model":    "echo",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	var body struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct{ Role, Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.Object != "chat.completion" || body.Model != "echo" || len(body.Choices) != 1 ||
		body.Choices[0].Message.Content != "You said: hello spillway" || body.Choices[0].Message.Role != "assistant" {
		t.Errorf("body = %s", rec.Body)
	}
}

func TestChat_AccessLog(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)

	e.do(t, request{auth: "Bearer " + keyCI, body: echoBody})

	log := e.logs.String()
	for _, want := range []string{"msg=request", "method=POST", "path=/v1/chat/completions", "status=200", "client_app=ci", "provider=mock", "model=echo", "latency_ms="} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	for _, never := range []string{"hello spillway", "You said", keyCI, "Bearer"} {
		if strings.Contains(log, never) {
			t.Errorf("log contains %q:\n%s", never, log)
		}
	}
}

func TestChat_ValidationErrors(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	bodies := map[string]string{
		"invalid JSON":     `{"model":`,
		"non-string model": `{"model":1,"messages":[{"role":"user","content":"hi"}]}`,
		"missing model":    `{"messages":[{"role":"user","content":"hi"}]}`,
		"missing messages": `{"model":"mock/echo"}`,
		"empty messages":   `{"model":"mock/echo","messages":[]}`,
		"trailing data":    echoBody + `{}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assertError(t, e.do(t, request{auth: "Bearer " + keyCI, body: body}), http.StatusBadRequest, "invalid_request_error", "invalid_request")
		})
	}
}

func TestChat_BodyTooLarge(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	body := `{"model":"mock/echo","messages":[{"role":"user","content":"` + strings.Repeat("x", 10<<20) + `"}]}`

	rec := e.do(t, request{auth: "Bearer " + keyCI, body: body})

	assertError(t, rec, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large")
}

func TestChat_BodyTooLargeStopsAtLimit(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	bodyText := `{"model":"mock/echo","messages":[{"role":"user","content":"` + strings.Repeat("x", 10<<20) + `"}]}`
	body := &countingBody{reader: strings.NewReader(bodyText)}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Authorization", "Bearer "+keyCI)
	recorder := httptest.NewRecorder()
	e.handler.ServeHTTP(recorder, req)
	assertError(t, recorder, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large")
	if body.read != (10<<20)+1 {
		t.Errorf("body bytes read = %d, want exactly 10 MiB + 1", body.read)
	}
}

type countingBody struct {
	reader *strings.Reader
	read   int
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read += n
	return n, err
}

func (b *countingBody) Close() error { return nil }

func TestChat_ModelNotFound(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	for _, model := range []string{"mock/nope", "unknown/x", "llama3"} {
		t.Run(model, func(t *testing.T) {
			t.Parallel()
			body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
			rec := e.do(t, request{auth: "Bearer " + keyCI, body: body})
			assertError(t, rec, http.StatusNotFound, "invalid_request_error", "model_not_found")
			if model != "mock/nope" && (!strings.Contains(rec.Body.String(), "provider/model") || !strings.Contains(rec.Body.String(), "GET /v1/models")) {
				t.Errorf("message = %s, want a model reference fix and model listing guidance", rec.Body)
			}
		})
	}
}

func TestChat_VirtualModelNotFoundPointsToRouterVersion(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	for _, model := range []string{"auto"} {
		t.Run(model, func(t *testing.T) {
			t.Parallel()
			body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
			rec := e.do(t, request{auth: "Bearer " + keyCI, body: body})
			assertError(t, rec, http.StatusNotFound, "invalid_request_error", "model_not_found")
			if !strings.Contains(rec.Body.String(), "v0.3") || !strings.Contains(rec.Body.String(), "GET /v1/models") || !strings.Contains(rec.Body.String(), "provider/model") {
				t.Errorf("message = %s, want router version and model listing guidance", rec.Body)
			}
		})
	}
}

func TestChat_TierSpillsInConfiguredOrder(t *testing.T) {
	t.Parallel()
	m, err := mock.New(mock.Options{Now: func() time.Time { return time.Unix(1760400000, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	set, err := providers.NewSet(m)
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	h, err := api.NewHandler(api.Options{
		Providers: set,
		LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
		Logger:    slog.New(slog.NewTextHandler(logs, nil)),
		Tiers: map[string][]providers.ModelRef{"demo": {
			{Provider: "mock", Model: "rate-limited"},
			{Provider: "mock", Model: "unavailable"},
			{Provider: "mock", Model: "echo"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(strings.Replace(echoBody, "mock/echo", "spillway/demo", 1)))
		req.Header.Set("Authorization", "Bearer "+keyCI)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
		}
		if rec.Header().Get("X-Spillway-Provider") != "mock" || rec.Header().Get("X-Spillway-Model") != "echo" {
			t.Errorf("spillway headers = %q/%q, want mock/echo", rec.Header().Get("X-Spillway-Provider"), rec.Header().Get("X-Spillway-Model"))
		}
		if got := rec.Header().Get("Retry-After"); got != "" {
			t.Errorf("Retry-After = %q, want empty after failed candidates", got)
		}
		if !strings.Contains(rec.Body.String(), `"model":"echo"`) {
			t.Errorf("response body does not identify echo: %s", rec.Body)
		}
	}
	if !strings.Contains(logs.String(), `tier=demo`) || !strings.Contains(logs.String(), `attempts=3`) {
		t.Errorf("logs do not identify tier and attempts: %s", logs)
	}
}

func TestChat_TierSpillsOnlyForProviderFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		err       error
		spill     bool
		wantCalls int32
	}{
		{name: "rate limited", err: providers.ErrRateLimited, spill: true, wantCalls: 1},
		{name: "payment required", err: providers.ErrPaymentRequired, spill: true, wantCalls: 1},
		{name: "server errors", err: providers.ErrUpstreamUnavailable, spill: true, wantCalls: 1},
		{name: "transport", err: fmt.Errorf("connection refused: %w", providers.ErrUpstreamUnavailable), spill: true, wantCalls: 1},
		{name: "timeout", err: fmt.Errorf("timeout: %w", providers.ErrUpstreamUnavailable), spill: true, wantCalls: 1},
		{name: "unauthorized", err: providers.ErrUnauthorized, spill: true, wantCalls: 1},
		{name: "forbidden", err: providers.ErrUnauthorized, spill: true, wantCalls: 1},
		{name: "not found", err: providers.ErrModelNotFound, spill: true, wantCalls: 1},
		{name: "client error", err: providers.ErrBadRequest, wantCalls: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := &countingProvider{name: "first", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
				return providers.ChatResponse{}, tt.err
			}}
			second := &countingProvider{name: "second", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
				return providers.ChatResponse{Body: json.RawMessage(`{"model":"second-model"}`)}, nil
			}}
			set, err := providers.NewSet(first, second)
			if err != nil {
				t.Fatal(err)
			}
			h, err := api.NewHandler(api.Options{
				Providers: set,
				LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
				Logger:    slog.New(slog.DiscardHandler),
				Tiers: map[string][]providers.ModelRef{"demo": {
					{Provider: "first", Model: "candidate"}, {Provider: "second", Model: "candidate"},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(strings.Replace(echoBody, "mock/echo", "spillway/demo", 1)))
			req.Header.Set("Authorization", "Bearer "+keyCI)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if got := second.calls.Load(); got != tt.wantCalls {
				t.Errorf("later candidate calls = %d, want %d", got, tt.wantCalls)
			}
			if tt.spill && (rec.Code != http.StatusOK || rec.Header().Get("X-Spillway-Provider") != "second") {
				t.Errorf("response = %d headers %v body %s, want second candidate success", rec.Code, rec.Header(), rec.Body)
			}
			if !tt.spill && rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestChat_TierDoesNotSpillOnUpstreamClientStatuses(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var upstreamCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":{"message":"invalid request"}}`)
			}))
			defer upstream.Close()
			first, err := openaicompat.New(openaicompat.Options{
				Name: "first", BaseURL: upstream.URL, APIKey: redact.NewSecret(keyCursor), Timeout: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			second := &countingProvider{name: "second", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
				return providers.ChatResponse{Body: json.RawMessage(`{"model":"second"}`)}, nil
			}}
			set, err := providers.NewSet(first, second)
			if err != nil {
				t.Fatal(err)
			}
			h, err := api.NewHandler(api.Options{
				Providers: set,
				LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
				Logger:    slog.New(slog.DiscardHandler),
				Tiers: map[string][]providers.ModelRef{"demo": {
					{Provider: "first", Model: "candidate"}, {Provider: "second", Model: "candidate"},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(strings.Replace(echoBody, "mock/echo", "spillway/demo", 1)))
			req.Header.Set("Authorization", "Bearer "+keyCI)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			assertError(t, rec, http.StatusBadRequest, "invalid_request_error", "provider_bad_request")
			if got := second.calls.Load(); got != 0 {
				t.Errorf("later candidate calls = %d for upstream %d, want 0", got, status)
			}
			if got := upstreamCalls.Load(); got != 1 {
				t.Errorf("upstream calls = %d, want 1", got)
			}
		})
	}
}

func TestChat_TierExhaustionIsBoundedAndDoesNotLeakProviderText(t *testing.T) {
	t.Parallel()
	first := &countingProvider{name: "first", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
		return providers.ChatResponse{}, fmt.Errorf("upstream response %s: %w", keyCursor, providers.ErrRateLimited)
	}}
	second := &countingProvider{name: "second", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
		return providers.ChatResponse{}, fmt.Errorf("private upstream body: %w", providers.ErrUnauthorized)
	}}
	set, err := providers.NewSet(first, second)
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	h, err := api.NewHandler(api.Options{
		Providers: set,
		LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
		Logger:    slog.New(slog.NewTextHandler(logs, nil)),
		Tiers: map[string][]providers.ModelRef{"demo": {
			{Provider: "first", Model: "one"}, {Provider: "second", Model: "two"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(strings.Replace(echoBody, "mock/echo", "spillway/demo", 1)))
	req.Header.Set("Authorization", "Bearer "+keyCI)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertError(t, rec, http.StatusServiceUnavailable, "api_error", "no_candidate_available")
	if len(rec.Body.Bytes()) >= 2<<10 {
		t.Errorf("error body is %d bytes, want under 2 KiB", len(rec.Body.Bytes()))
	}
	for _, never := range []string{keyCI, keyCursor, "private upstream body", "upstream response"} {
		if strings.Contains(rec.Body.String()+logs.String(), never) {
			t.Errorf("response or logs leak %q: body=%s logs=%s", never, rec.Body, logs)
		}
	}
	for _, want := range []string{"first/one rate limited", "second/two unauthorized"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("exhaustion message lacks %q: %s", want, rec.Body)
		}
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q after exhaustion, want empty", got)
	}
	for _, want := range []string{"tier=demo", "attempts=2", "error_kind=rate_limited", "error_kind=unauthorized"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs lack %q: %s", want, logs)
		}
	}
	if got := strings.Count(logs.String(), `msg="tier candidate failed"`); got != 2 {
		t.Errorf("failed-attempt log lines = %d, want 2: %s", got, logs)
	}
}

func TestChat_TierCooldownExhaustionUsesEarliestRetryAfter(t *testing.T) {
	first := &countingProvider{name: "first", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
		return providers.ChatResponse{}, providers.RetryAfterError{After: 34 * time.Second, Kind: providers.ErrRateLimited}
	}}
	second := &countingProvider{name: "second", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
		return providers.ChatResponse{}, providers.RetryAfterError{After: 7 * time.Second, Kind: providers.ErrRateLimited}
	}}
	set, err := providers.NewSet(first, second)
	if err != nil {
		t.Fatal(err)
	}
	refs := []providers.ModelRef{{Provider: "first", Model: "candidate"}, {Provider: "second", Model: "candidate"}}
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	tracker, err := quota.New(quota.Options{Now: func() time.Time { return now }, Candidates: refs})
	if err != nil {
		t.Fatal(err)
	}
	h, err := api.NewHandler(api.Options{
		Providers: set,
		Tracker:   tracker,
		Now:       func() time.Time { return now },
		LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
		Logger:    slog.New(slog.DiscardHandler),
		Tiers:     map[string][]providers.ModelRef{"demo": refs},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(strings.Replace(echoBody, "mock/echo", "spillway/demo", 1)))
		req.Header.Set("Authorization", "Bearer "+keyCI)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assertError(t, rec, http.StatusTooManyRequests, "api_error", "rate_limit_exceeded")
		retry, parseErr := strconv.Atoi(rec.Header().Get("Retry-After"))
		if parseErr != nil || retry != 7 {
			t.Errorf("Retry-After = %q, want earliest cooldown of 7", rec.Header().Get("Retry-After"))
		}
		for _, candidate := range []string{"first/candidate rate limited (retry in 34s)", "second/candidate rate limited (retry in 7s)"} {
			if !strings.Contains(rec.Body.String(), candidate) {
				t.Errorf("cooldown reason %q missing from body: %s", candidate, rec.Body)
			}
		}
	}
	if got := first.calls.Load(); got != 1 {
		t.Errorf("first provider calls = %d, want first request only", got)
	}
	if got := second.calls.Load(); got != 1 {
		t.Errorf("second provider calls = %d, want first request only", got)
	}
}

func TestChat_MockDeadTierOpensCircuitAfterThreeCalls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, err := mock.New(mock.Options{Now: time.Now})
		if err != nil {
			t.Fatal(err)
		}
		set, err := providers.NewSet(m)
		if err != nil {
			t.Fatal(err)
		}
		refs := []providers.ModelRef{{Provider: "mock", Model: "rate-limited"}, {Provider: "mock", Model: "unavailable"}}
		tracker, err := quota.New(quota.Options{Now: time.Now, Candidates: refs})
		if err != nil {
			t.Fatal(err)
		}
		h, err := api.NewHandler(api.Options{
			Providers: set,
			Tracker:   tracker,
			LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
			Logger:    slog.New(slog.DiscardHandler),
			Tiers:     map[string][]providers.ModelRef{"dead": refs},
		})
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(strings.Replace(echoBody, "mock/echo", "spillway/dead", 1)))
			req.Header.Set("Authorization", "Bearer "+keyCI)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("call status = %d, want 503 before all candidates cool; body %s", rec.Code, rec.Body)
			}
		}
		for range 2 {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(strings.Replace(echoBody, "mock/echo", "spillway/dead", 1)))
			req.Header.Set("Authorization", "Bearer "+keyCI)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			assertError(t, rec, http.StatusTooManyRequests, "api_error", "rate_limit_exceeded")
			if !strings.Contains(rec.Body.String(), "mock/unavailable circuit open (retry in 30s)") {
				t.Errorf("exhaustion body = %s, want open circuit reason", rec.Body)
			}
		}
	})
}

func TestChat_TierCooldownHandlesConcurrentRequests(t *testing.T) {
	first := &countingProvider{name: "first", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
		return providers.ChatResponse{}, providers.ErrRateLimited
	}}
	second := &countingProvider{name: "second", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
		return providers.ChatResponse{Body: json.RawMessage(`{"model":"second"}`)}, nil
	}}
	set, err := providers.NewSet(first, second)
	if err != nil {
		t.Fatal(err)
	}
	refs := []providers.ModelRef{{Provider: "first", Model: "candidate"}, {Provider: "second", Model: "candidate"}}
	tracker, err := quota.New(quota.Options{Candidates: refs})
	if err != nil {
		t.Fatal(err)
	}
	h, err := api.NewHandler(api.Options{
		Providers: set,
		Tracker:   tracker,
		LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
		Logger:    slog.New(slog.DiscardHandler),
		Tiers:     map[string][]providers.ModelRef{"demo": refs},
	})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(strings.Replace(echoBody, "mock/echo", "spillway/demo", 1)))
			req.Header.Set("Authorization", "Bearer "+keyCI)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("concurrent response status = %d, want 200; body %s", rec.Code, rec.Body)
			}
		}()
	}
	wg.Wait()
}

func TestChat_TierGroqQuotaHintSkipsUntilReset(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		firstSuccesses := 0
		first := &countingProvider{name: "first", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
			hint := providers.RateLimit{}
			firstSuccesses++
			if firstSuccesses == 1 {
				hint = providers.RateLimit{Known: true, RemainingRequests: 0, ResetRequests: 2*time.Minute + 59*time.Second + 560*time.Millisecond}
			}
			return providers.ChatResponse{Body: json.RawMessage(`{"model":"first"}`), RateLimit: hint}, nil
		}}
		second := &countingProvider{name: "second", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
			return providers.ChatResponse{Body: json.RawMessage(`{"model":"second"}`)}, nil
		}}
		set, err := providers.NewSet(first, second)
		if err != nil {
			t.Fatal(err)
		}
		refs := []providers.ModelRef{{Provider: "first", Model: "candidate"}, {Provider: "second", Model: "candidate"}}
		tracker, err := quota.New(quota.Options{Now: time.Now, Candidates: refs})
		if err != nil {
			t.Fatal(err)
		}
		h, err := api.NewHandler(api.Options{
			Providers: set,
			Tracker:   tracker,
			LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
			Logger:    slog.New(slog.DiscardHandler),
			Tiers:     map[string][]providers.ModelRef{"demo": refs},
		})
		if err != nil {
			t.Fatal(err)
		}
		request := func() *httptest.ResponseRecorder {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(strings.Replace(echoBody, "mock/echo", "spillway/demo", 1)))
			req.Header.Set("Authorization", "Bearer "+keyCI)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			return rec
		}
		if rec := request(); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"model":"first"`) {
			t.Fatalf("first response = %d %s", rec.Code, rec.Body)
		}
		<-time.After(179*time.Second + 559*time.Millisecond)
		if rec := request(); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"model":"second"`) {
			t.Fatalf("cooldown response = %d %s; want second candidate before reset", rec.Code, rec.Body)
		}
		if first.calls.Load() != 1 {
			t.Fatalf("first candidate calls before reset = %d, want 1", first.calls.Load())
		}
		<-time.After(time.Millisecond)
		if rec := request(); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"model":"first"`) {
			t.Fatalf("reset response = %d %s; want first candidate at reset", rec.Code, rec.Body)
		}
		if first.calls.Load() != 2 {
			t.Fatalf("first candidate calls after reset = %d, want 2", first.calls.Load())
		}
	})
}

func TestChat_TierUnknownNamesConfiguredTiersAndAutoStaysUnavailable(t *testing.T) {
	t.Parallel()
	m, err := mock.New(mock.Options{Now: func() time.Time { return time.Unix(1760400000, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	set, err := providers.NewSet(m)
	if err != nil {
		t.Fatal(err)
	}
	h, err := api.NewHandler(api.Options{
		Providers: set,
		LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
		Logger:    slog.New(slog.DiscardHandler),
		Tiers:     map[string][]providers.ModelRef{"demo": {{Provider: "mock", Model: "echo"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]string{"spillway/missing": "demo", "auto": "v0.3"} {
		t.Run(model, func(t *testing.T) {
			body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+keyCI)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			assertError(t, rec, http.StatusNotFound, "invalid_request_error", "model_not_found")
			if !strings.Contains(rec.Body.String(), want) {
				t.Errorf("message = %s, want %q", rec.Body, want)
			}
		})
	}
}

func TestChat_TierCancellationStopsFurtherCandidates(t *testing.T) {
	started := make(chan struct{})
	first := &cancelingProvider{started: started}
	second := &countingProvider{name: "second", chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
		return providers.ChatResponse{Body: json.RawMessage(`{"model":"second"}`)}, nil
	}}
	set, err := providers.NewSet(first, second)
	if err != nil {
		t.Fatal(err)
	}
	h, err := api.NewHandler(api.Options{
		Providers: set,
		LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
		Logger:    slog.New(slog.DiscardHandler),
		Tiers: map[string][]providers.ModelRef{"demo": {
			{Provider: "cancel", Model: "blocked"}, {Provider: "second", Model: "candidate"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(strings.Replace(echoBody, "mock/echo", "spillway/demo", 1)))
	req.Header.Set("Authorization", "Bearer "+keyCI)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.ServeHTTP(rec, req); close(done) }()
	<-started
	cancel()
	<-done
	if got := second.calls.Load(); got != 0 {
		t.Errorf("later candidate calls = %d after cancellation, want 0", got)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("response body = %s after cancellation, want empty", rec.Body)
	}
}

func TestChat_ModelReferencePreservesUpstreamSlashAndColon(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	var got string
	e.fake.chat = func(req providers.ChatRequest) (providers.ChatResponse, error) {
		got = req.Model
		return providers.ChatResponse{Body: json.RawMessage(`{"object":"chat.completion"}`)}, nil
	}

	rec := e.do(t, request{auth: "Bearer " + keyCI, body: `{"model":"fake/org/model:v1","messages":[{"role":"user","content":"hi"}]}`})

	if rec.Code != http.StatusOK || got != "org/model:v1" {
		t.Errorf("response = %d, upstream model = %q; want 200 and org/model:v1", rec.Code, got)
	}
}

func TestModels_ListsConfiguredModelsInStableOrder(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.fake.models = func(context.Context) ([]providers.Model, error) {
		return []providers.Model{{ID: "available"}}, nil
	}

	rec := e.do(t, request{method: http.MethodGet, path: "/v1/models", auth: "Bearer " + keyCI})

	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("response = %d %q %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Object != "list" || len(body.Data) != 6 {
		t.Fatalf("list = %+v, want object=list and six models", body)
	}
	want := []string{"fake/available", "mock/echo", "mock/rate-limited", "mock/stream-error", "mock/unauthorized", "mock/unavailable"}
	for i, model := range body.Data {
		if model.ID != want[i] || model.Object != "model" || model.OwnedBy == "" {
			t.Errorf("data[%d] = %+v, want id %q and OpenAI model fields", i, model, want[i])
		}
	}
}

func TestModels_ListsSortedVirtualTiers(t *testing.T) {
	t.Parallel()
	m, err := mock.New(mock.Options{Now: func() time.Time { return time.Unix(1760400000, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	set, err := providers.NewSet(m)
	if err != nil {
		t.Fatal(err)
	}
	h, err := api.NewHandler(api.Options{
		Providers: set,
		LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
		Logger:    slog.New(slog.DiscardHandler),
		Tiers: map[string][]providers.ModelRef{
			"z-tier": {{Provider: "mock", Model: "echo"}},
			"a-tier": {{Provider: "mock", Model: "echo"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+keyCI)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	var body struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var tiers []string
	for _, item := range body.Data {
		if strings.HasPrefix(item.ID, "spillway/") {
			if item.OwnedBy != "spillway" {
				t.Errorf("%s owned_by = %q, want spillway", item.ID, item.OwnedBy)
			}
			tiers = append(tiers, item.ID)
		}
	}
	if got := strings.Join(tiers, ","); got != "spillway/a-tier,spillway/z-tier" {
		t.Errorf("virtual tiers = %q, want sorted tier IDs", got)
	}
}

func TestModels_QueriesProvidersConcurrently(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{}, 2)
		release := make(chan struct{})
		var timedOut atomic.Int32
		provider := func(name string) *countingProvider {
			return &countingProvider{
				name: name,
				models: func(ctx context.Context) ([]providers.Model, error) {
					started <- struct{}{}
					select {
					case <-release:
						return []providers.Model{{ID: name}}, nil
					case <-ctx.Done():
						timedOut.Add(1)
						return nil, ctx.Err()
					}
				},
				chat:   func(providers.ChatRequest) (providers.ChatResponse, error) { return providers.ChatResponse{}, nil },
				stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) { return nil, nil },
			}
		}
		set, err := providers.NewSet(provider("alpha"), provider("beta"))
		if err != nil {
			t.Fatal(err)
		}
		h, err := api.NewHandler(api.Options{
			Providers:     set,
			LocalKeys:     []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
			Logger:        slog.New(slog.DiscardHandler),
			ModelsTimeout: 5 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+keyCI)
		rec := httptest.NewRecorder()
		done := make(chan struct{})
		go func() { h.ServeHTTP(rec, req); close(done) }()
		for range 2 {
			select {
			case <-started:
			case <-time.After(6 * time.Second):
				cancel()
				t.Fatal("providers were not queried concurrently")
			}
		}
		if got := timedOut.Load(); got != 0 {
			cancel()
			t.Fatalf("providers timed out before both started: %d", got)
		}
		close(release)
		<-done
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "alpha/alpha") || !strings.Contains(rec.Body.String(), "beta/beta") {
			t.Fatalf("response = %d %s; want both provider model lists", rec.Code, rec.Body)
		}
	})
}

func TestModels_TimesOutSlowProvider(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stalled := &countingProvider{
			name: "stalled",
			models: func(ctx context.Context) ([]providers.Model, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
			chat:   func(providers.ChatRequest) (providers.ChatResponse, error) { return providers.ChatResponse{}, nil },
			stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) { return nil, nil },
		}
		healthy := &countingProvider{
			name: "healthy",
			models: func(context.Context) ([]providers.Model, error) {
				return []providers.Model{{ID: "ready"}}, nil
			},
			chat:   func(providers.ChatRequest) (providers.ChatResponse, error) { return providers.ChatResponse{}, nil },
			stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) { return nil, nil },
		}
		set, err := providers.NewSet(stalled, healthy)
		if err != nil {
			t.Fatal(err)
		}
		h, err := api.NewHandler(api.Options{
			Providers:     set,
			LocalKeys:     []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
			Logger:        slog.New(slog.DiscardHandler),
			ModelsTimeout: time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+keyCI)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "healthy/ready") || strings.Contains(rec.Body.String(), "stalled/") {
			t.Fatalf("response = %d %s; want healthy model and stalled provider omitted", rec.Code, rec.Body)
		}
	})
}

func TestModels_RequiresLocalKey(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)

	assertError(t, e.do(t, request{method: http.MethodGet, path: "/v1/models"}), http.StatusUnauthorized, "authentication_error", "invalid_api_key")
}

func TestModels_PartialFailureLogsScrubbedWarning(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.fake.models = func(context.Context) ([]providers.Model, error) {
		return nil, errors.New("provider failed with sk-test-provider-secret")
	}

	rec := e.do(t, request{method: http.MethodGet, path: "/v1/models", auth: "Bearer " + keyCI})

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "mock/echo") {
		t.Errorf("response = %d %s, want partial model list", rec.Code, rec.Body)
	}
	log := e.logs.String()
	if !strings.Contains(log, "level=WARN") || !strings.Contains(log, "provider=fake") || strings.Contains(log, "sk-test-provider-secret") {
		t.Errorf("log = %s, want scrubbed WARN for fake provider", log)
	}
}

func TestModels_AllProvidersFailReturnsBadGateway(t *testing.T) {
	t.Parallel()
	fail := func(name string) *countingProvider {
		return &countingProvider{
			name: name,
			models: func(context.Context) ([]providers.Model, error) {
				return nil, errors.New("provider failed")
			},
			chat: func(providers.ChatRequest) (providers.ChatResponse, error) {
				return providers.ChatResponse{}, nil
			},
			stream: func(context.Context, providers.ChatRequest) (providers.Stream, error) {
				return nil, nil
			},
		}
	}
	set, err := providers.NewSet(fail("alpha"), fail("beta"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := api.NewHandler(api.Options{
		Providers: set,
		LocalKeys: []api.LocalKey{{Name: "ci", Key: redact.NewSecret(keyCI)}},
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+keyCI)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertError(t, rec, http.StatusBadGateway, "api_error", "upstream_unavailable")
}

// A client mistake is not a Spillway failure, so it must not log at error level.
func TestChat_ClientErrorLoggedAtInfo(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)

	e.do(t, request{auth: "Bearer " + keyCI, body: `{"model":"mock/nope","messages":[{"role":"user","content":"hi"}]}`})

	if log := e.logs.String(); !strings.Contains(log, "level=INFO") || strings.Contains(log, "level=ERROR") {
		t.Errorf("log = %s, want one INFO line", log)
	}
}

func TestChat_ProviderFailureIsInternalError(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.fake.chat = func(providers.ChatRequest) (providers.ChatResponse, error) {
		return providers.ChatResponse{}, errors.New("boom")
	}

	rec := e.do(t, request{auth: "Bearer " + keyCI, body: `{"model":"fake/x","messages":[{"role":"user","content":"hi"}]}`})

	assertError(t, rec, http.StatusInternalServerError, "api_error", "internal_error")
	if strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("body leaks the internal error: %s", rec.Body)
	}
	if !strings.Contains(e.logs.String(), "boom") {
		t.Errorf("log lacks the error:\n%s", e.logs)
	}
}

func TestChat_PaymentRequiredMapsToInsufficientQuota(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.fake.chat = func(providers.ChatRequest) (providers.ChatResponse, error) {
		return providers.ChatResponse{}, fmt.Errorf("upstream message %s: %w", keyCursor, providers.RetryAfterError{After: 12 * time.Second, Kind: providers.ErrPaymentRequired})
	}

	rec := e.do(t, request{auth: "Bearer " + keyCI, body: `{"model":"fake/x","messages":[{"role":"user","content":"hi"}]}`})

	assertError(t, rec, http.StatusTooManyRequests, "insufficient_quota", "insufficient_quota")
	if got := rec.Header().Get("Retry-After"); got != "12" {
		t.Errorf("Retry-After = %q, want 12", got)
	}
	if strings.Contains(rec.Body.String(), keyCursor) {
		t.Errorf("response exposes provider key: %s", rec.Body)
	}
}

func TestRecover_PanicReturns500(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.fake.chat = func(providers.ChatRequest) (providers.ChatResponse, error) { panic("kaboom") }

	rec := e.do(t, request{auth: "Bearer " + keyCI, body: `{"model":"fake/x","messages":[{"role":"user","content":"hi"}]}`})

	assertError(t, rec, http.StatusInternalServerError, "api_error", "internal_error")
	if !strings.Contains(e.logs.String(), "kaboom") {
		t.Errorf("log lacks the panic:\n%s", e.logs)
	}
	if rec := e.do(t, request{auth: "Bearer " + keyCI, body: echoBody}); rec.Code != http.StatusOK {
		t.Errorf("next request: status = %d, want 200", rec.Code)
	}
}

func TestRoutes_NotFoundAndMethodNotAllowed(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)

	assertError(t, e.do(t, request{method: http.MethodGet, path: "/v1/nope", auth: "Bearer " + keyCI}), http.StatusNotFound, "invalid_request_error", "not_found")

	rec := e.do(t, request{method: http.MethodGet, auth: "Bearer " + keyCI})
	assertError(t, rec, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed")
	if got := rec.Header().Get("Allow"); got != "POST" {
		t.Errorf("Allow = %q, want POST", got)
	}
}

func TestNewServer_Timeouts(t *testing.T) {
	t.Parallel()
	srv := api.NewServer(http.NotFoundHandler(), slog.New(slog.DiscardHandler))
	if srv.ReadHeaderTimeout <= 0 || srv.IdleTimeout <= 0 || srv.MaxHeaderBytes <= 0 {
		t.Errorf("ReadHeaderTimeout %v, IdleTimeout %v, MaxHeaderBytes %d; want all set", srv.ReadHeaderTimeout, srv.IdleTimeout, srv.MaxHeaderBytes)
	}
	// Server-wide read and write timeouts would cut long streams.
	if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 {
		t.Errorf("ReadTimeout %v, WriteTimeout %v; want 0", srv.ReadTimeout, srv.WriteTimeout)
	}
	if srv.ErrorLog == nil {
		t.Error("ErrorLog is nil, want it bridged to slog")
	}
}
