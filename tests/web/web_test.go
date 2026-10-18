package web_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/Polqt/spillway/apps/spillway/web"
)

func TestHandler_ServesEmbeddedDashboardAndSameOriginAssets(t *testing.T) {
	t.Parallel()
	handler := web.Handler()

	page := getAsset(t, handler, "/dashboard")
	if strings.Contains(page, "private-model-marker") || strings.Contains(page, "usage-data-marker") {
		t.Fatal("unauthenticated dashboard shell contains usage data")
	}
	assets := validateDashboardMarkup(t, page)
	if len(assets) != 2 {
		t.Fatalf("dashboard references %d bundled assets, want JavaScript and CSS", len(assets))
	}
	var dashboardContent strings.Builder
	dashboardContent.WriteString(page)
	for _, asset := range assets {
		if !strings.HasPrefix(asset, "/assets/") && asset != "/dashboard.css" && asset != "/dashboard.js" {
			t.Errorf("dashboard asset URL = %q, want a supported same-origin asset path", asset)
		}
		if body := getAsset(t, handler, asset); body == "" {
			t.Errorf("GET %s returned an empty body", asset)
		} else {
			dashboardContent.WriteString(body)
		}
	}
	for _, copy := range []string{"dashboard: true", "without", "session storage", "OpenAI-compatible client", "/api/providers", "does not probe providers", "credential_required"} {
		if !strings.Contains(dashboardContent.String(), copy) {
			t.Errorf("dashboard onboarding is missing %q", copy)
		}
	}
}

func TestHandler_DashboardHasSecurityHeadersAndOnboarding(t *testing.T) {
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
	page := recorder.Body.String()
	validateDashboardMarkup(t, page)
}

func validateDashboardMarkup(t *testing.T, markup string) []string {
	t.Helper()
	for _, forbidden := range []string{"http://", "https://", "<style", "<script>"} {
		if strings.Contains(strings.ToLower(markup), forbidden) {
			t.Errorf("dashboard shell contains external or inline resource %q", forbidden)
		}
	}
	var assets []string
	for _, element := range regexp.MustCompile(`<(?:script|link)\b[^>]*>`).FindAllString(markup, -1) {
		for _, match := range regexp.MustCompile(`(?:src|href)="([^"]+)"`).FindAllStringSubmatch(element, -1) {
			value := match[1]
			parsed, err := url.Parse(value)
			if err != nil || parsed.IsAbs() || parsed.Host != "" {
				t.Errorf("dashboard contains non-local URL %q", value)
			}
			assets = append(assets, value)
		}
	}
	if strings.Contains(markup, " onsubmit=") || strings.Contains(markup, " onclick=") || strings.Contains(markup, " style=") {
		t.Fatal("dashboard contains inline event handler or style")
	}
	return assets
}

func getAsset(t *testing.T, handler http.Handler, path string) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s returned %d: %s", path, recorder.Code, recorder.Body)
	}
	if recorder.Body.Len() == 0 {
		t.Fatalf("GET %s returned an empty body", path)
	}
	return recorder.Body.String()
}
