package cli

import (
	"io"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
)

// The Spillway palette runs from water teal to dusk violet.
var (
	colorTeal   = lipgloss.Color("#2DD4BF")
	colorViolet = lipgloss.Color("#8B5CF6")
	colorGreen  = lipgloss.Color("#34D399")
	colorAmber  = lipgloss.Color("#FBBF24")
	colorRed    = lipgloss.Color("#FB7185")
	colorMuted  = lipgloss.Color("244")
	colorFaint  = lipgloss.Color("238")

	mutedStyle = lipgloss.NewStyle().Foreground(colorMuted)
	okStyle    = lipgloss.NewStyle().Foreground(colorGreen)
	warnStyle  = lipgloss.NewStyle().Foreground(colorAmber)
	errStyle   = lipgloss.NewStyle().Foreground(colorRed)
	tealStyle  = lipgloss.NewStyle().Foreground(colorTeal)
)

// styledWriter downsamples colors for w. Unlike lipgloss.Fprint, NO_COLOR
// also wins over CLICOLOR_FORCE when output is redirected.
func styledWriter(w io.Writer) io.Writer {
	out := colorprofile.NewWriter(w, os.Environ())
	if os.Getenv("NO_COLOR") != "" && out.Profile > colorprofile.ASCII {
		out.Profile = colorprofile.ASCII
	}
	return out
}

// gradient paints text from teal to violet, one color per rune.
func gradient(text string) string {
	runes := []rune(text)
	if len(runes) == 0 {
		return ""
	}
	colors := lipgloss.Blend1D(len(runes), colorTeal, colorViolet)
	var out strings.Builder
	for i, r := range runes {
		out.WriteString(lipgloss.NewStyle().Bold(true).Foreground(colors[i]).Render(string(r)))
	}
	return out.String()
}

// brandHeader is the wave mark, wordmark and a dim context label.
func brandHeader(context string) string {
	line := gradient("∿∿∿") + " " + gradient("spillway")
	if context != "" {
		line += "  " + mutedStyle.Render(context)
	}
	return line
}
