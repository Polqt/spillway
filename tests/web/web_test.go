package web_test

import (
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Polqt/spillway/apps/spillway/web"
)

func TestHandler_ServesEmbeddedDashboardAssets(t *testing.T) {
	t.Parallel()
	handler := web.Handler()

	for _, path := range []string{"/dashboard", "/dashboard.css", "/dashboard.js"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d, want 200", path, recorder.Code)
			}
			if recorder.Body.Len() == 0 {
				t.Fatalf("GET %s returned an empty body", path)
			}
		})
	}
}

func TestHandler_DashboardHasSecurityHeadersAndSafeAssets(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	web.Handler().ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/dashboard", nil))

	if got := recorder.Header().Get("Content-Security-Policy"); got != "default-src 'self'; frame-ancestors 'none'; base-uri 'none'" {
		t.Errorf("Content-Security-Policy = %q", got)
	}
	if got := recorder.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := recorder.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", got)
	}
	validateDashboardMarkup(t, recorder.Body.String())

	js := getAsset(t, "/dashboard.js")
	for _, want := range []string{"sessionStorage", "Authorization", "Bearer", "textContent", "10000", "cannot read the dashboard", "est."} {
		if !strings.Contains(js, want) {
			t.Errorf("dashboard script missing %q", want)
		}
	}
	for _, forbidden := range []string{"document.cookie", "innerHTML", "outerHTML", "insertAdjacentHTML", "document.write(", "http://", "https://"} {
		if strings.Contains(js, forbidden) {
			t.Errorf("dashboard script contains unsafe or external API %q", forbidden)
		}
	}
}

func validateDashboardMarkup(t *testing.T, markup string) {
	t.Helper()
	decoder := xml.NewDecoder(strings.NewReader(markup))
	scriptCount, stylesheetCount := 0, 0
	keyFormPost := false
	inScript := false
	for {
		token, err := decoder.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("dashboard HTML is not parseable: %v", err)
		}
		switch element := token.(type) {
		case xml.StartElement:
			inScript = strings.EqualFold(element.Name.Local, "script")
			if strings.EqualFold(element.Name.Local, "form") {
				for _, attr := range element.Attr {
					if strings.EqualFold(attr.Name.Local, "method") && strings.EqualFold(attr.Value, "post") {
						keyFormPost = true
					}
				}
			}
			if strings.EqualFold(element.Name.Local, "style") {
				t.Fatal("dashboard contains an inline style element")
			}
			var source string
			var relation string
			var target string
			for _, attr := range element.Attr {
				name := strings.ToLower(attr.Name.Local)
				switch name {
				case "src":
					source = attr.Value
				case "rel":
					relation = strings.ToLower(attr.Value)
				case "href":
					target = attr.Value
				case "style":
					t.Fatal("dashboard contains an inline style attribute")
				}
				if strings.HasPrefix(name, "on") {
					t.Fatalf("dashboard contains inline event handler %q", name)
				}
				switch name {
				case "src", "href", "action", "formaction", "poster", "cite", "data":
					parsed, parseErr := url.Parse(attr.Value)
					if parseErr != nil || parsed.IsAbs() || parsed.Host != "" {
						t.Fatalf("dashboard contains external URL %q", attr.Value)
					}
				}
			}
			if inScript {
				scriptCount++
				if source != "/dashboard.js" {
					t.Errorf("script source = %q, want /dashboard.js", source)
				}
			}
			if strings.EqualFold(element.Name.Local, "link") && relation == "stylesheet" {
				stylesheetCount++
				if target != "/dashboard.css" {
					t.Errorf("stylesheet URL = %q, want /dashboard.css", target)
				}
			}
		case xml.CharData:
			if inScript && strings.TrimSpace(string(element)) != "" {
				t.Fatal("dashboard contains inline script content")
			}
		case xml.EndElement:
			if strings.EqualFold(element.Name.Local, "script") {
				inScript = false
			}
		}
	}
	if scriptCount != 1 || stylesheetCount != 1 || !keyFormPost {
		t.Errorf("dashboard has %d external scripts, %d stylesheets and key form POST=%t, want one of each and a POST form", scriptCount, stylesheetCount, keyFormPost)
	}
}

func getAsset(t *testing.T, path string) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	web.Handler().ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d", path, recorder.Code)
	}
	return recorder.Body.String()
}
