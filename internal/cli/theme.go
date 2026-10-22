package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
)

// The Spillway palette runs from water teal to dusk violet.
var (
	colorTeal   = lipgloss.Color("#2DD4BF")
	colorViolet = lipgloss.Color("#8B5CF6")
	colorPink   = lipgloss.Color("#F472B6")
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

// clearScreenSequence moves the cursor home and erases the visible screen.
const clearScreenSequence = "\x1b[H\x1b[2J"

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

func writeBrandHeader(w io.Writer, context, tagline string) error {
	text := "\n" + brandHeader(context) + "\n"
	if tagline != "" {
		text += mutedStyle.Render(tagline) + "\n"
	}
	_, err := fmt.Fprintln(styledWriter(w), text)
	return err
}

// receipt is the one-line record an answered prompt collapses into.
func receipt(title, answer string) string {
	return okStyle.Bold(true).Render("✔") + " " +
		lipgloss.NewStyle().Bold(true).Render(title) + " " +
		mutedStyle.Render("…") + " " +
		tealStyle.Render(answer)
}

func spillwayFormTheme() huh.Theme {
	return huh.ThemeFunc(func(isDark bool) *huh.Styles {
		t := huh.ThemeCharm(isDark)
		lightDark := lipgloss.LightDark(isDark)
		text := lightDark(lipgloss.Color("235"), lipgloss.Color("252"))

		t.Focused.Base = t.Focused.Base.BorderForeground(colorViolet)
		t.Focused.Title = lipgloss.NewStyle().Foreground(colorTeal).Bold(true).SetString("?")
		t.Focused.Description = mutedStyle
		t.Focused.SelectSelector = lipgloss.NewStyle().Foreground(colorPink).SetString("❯ ")
		t.Focused.MultiSelectSelector = t.Focused.SelectSelector
		t.Focused.Option = lipgloss.NewStyle().Foreground(text)
		t.Focused.UnselectedOption = t.Focused.Option
		t.Focused.SelectedOption = lipgloss.NewStyle().Foreground(colorTeal)
		t.Focused.SelectedPrefix = lipgloss.NewStyle().Foreground(colorGreen).SetString("◉ ")
		t.Focused.UnselectedPrefix = lipgloss.NewStyle().Foreground(colorMuted).SetString("○ ")
		t.Focused.FocusedButton = t.Focused.FocusedButton.Foreground(lipgloss.Color("#0B1020")).Background(colorTeal).Bold(true)
		t.Focused.BlurredButton = t.Focused.BlurredButton.Foreground(text).Background(lightDark(lipgloss.Color("252"), colorFaint))
		t.Focused.Next = t.Focused.FocusedButton
		t.Focused.TextInput.Prompt = lipgloss.NewStyle().Foreground(colorPink)
		t.Focused.TextInput.Cursor = lipgloss.NewStyle().Foreground(colorTeal)
		t.Focused.ErrorIndicator = errStyle
		t.Focused.ErrorMessage = errStyle
		t.Help.ShortKey = mutedStyle
		t.Help.ShortDesc = lipgloss.NewStyle().Foreground(colorFaint)
		t.Help.ShortSeparator = lipgloss.NewStyle().Foreground(colorFaint)

		t.Blurred = t.Focused
		t.Blurred.Base = t.Focused.Base.BorderStyle(lipgloss.HiddenBorder())
		t.Blurred.Card = t.Blurred.Base
		t.Blurred.NextIndicator = lipgloss.NewStyle()
		t.Blurred.PrevIndicator = lipgloss.NewStyle()
		t.Group.Title = t.Focused.Title
		t.Group.Description = t.Focused.Description
		return t
	})
}
