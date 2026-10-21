//go:build dashboardassets

package web_test

import (
	"testing"

	"github.com/Polqt/spillway/apps/spillway/web"
)

func TestAssets_EmbeddedBuildReportsEmbedded(t *testing.T) {
	t.Parallel()
	if got := web.Assets(); got != "embedded" {
		t.Errorf("Assets() = %q, want embedded", got)
	}
}
