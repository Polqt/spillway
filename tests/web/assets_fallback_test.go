//go:build !dashboardassets

package web_test

import (
	"testing"

	"github.com/Polqt/spillway/apps/spillway/web"
)

func TestAssets_FallbackBuildReportsFallback(t *testing.T) {
	t.Parallel()
	if got := web.Assets(); got != "fallback" {
		t.Errorf("Assets() = %q, want fallback", got)
	}
}
