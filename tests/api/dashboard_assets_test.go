package api_test

import (
	"net/http"
	"regexp"
	"testing"
)

func TestDashboardAssets_AreUnauthenticatedButHostChecked(t *testing.T) {
	handler, _, _ := dashboardEnv(t)
	shell := dashboardRequest(t, handler, "/dashboard", "", "127.0.0.1")
	assets := regexp.MustCompile(`(?:src|href)="((?:/assets/[^"]+|/dashboard\.(?:css|js)))"`).FindAllStringSubmatch(shell.Body.String(), -1)
	if len(assets) == 0 {
		t.Fatal("dashboard shell does not reference embedded assets")
	}
	for _, asset := range assets {
		response := dashboardRequest(t, handler, asset[1], "", "127.0.0.1")
		if response.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200: %s", asset[1], response.Code, response.Body)
		}
		if got := response.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("GET %s Cache-Control = %q, want no-store", asset[1], got)
		}
		foreign := dashboardRequest(t, handler, asset[1], "", "attacker.example")
		if foreign.Code != http.StatusForbidden {
			t.Errorf("foreign Host GET %s status = %d, want 403", asset[1], foreign.Code)
		}
	}
}
