package fakenet_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Polqt/spillway/internal/fakenet"
)

func TestHTTPRoundTrip(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		listener := fakenet.Listen()
		server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "ok")
		})}
		serveDone := make(chan error, 1)
		go func() { serveDone <- server.Serve(listener) }()

		transport := &http.Transport{DialContext: listener.DialContext}
		client := &http.Client{Transport: transport}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://fakenet.test/", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || string(body) != "ok" {
			t.Fatalf("response body = %q, %v; want ok", body, err)
		}
		client.CloseIdleConnections()
		_ = server.Close()
		if err := <-serveDone; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve() = %v, want http.ErrServerClosed", err)
		}
	})
}

func TestDialContext_Cancelled(t *testing.T) {
	listener := fakenet.Listen()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := listener.DialContext(ctx, "", ""); err == nil {
		t.Fatal("DialContext() succeeded with cancelled context")
	}
	_ = listener.Close()
}
