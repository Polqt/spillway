package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"
	"github.com/spf13/cobra"
)

const (
	helpMinimumWidth   = 48
	helpMinimumHeight  = 13
	helpWideBreakpoint = 82
)

type helpEntry struct {
	name       string
	group      string
	groupOrder int
	short      string
	detail     string
}

type helpModel struct {
	title    string
	subtitle string
	entries  []helpEntry
	search   textinput.Model
	selected int
	scroll   int
	width    int
	height   int
	showHelp bool
}

func runHelpExplorer(ctx context.Context, root *cobra.Command, input io.Reader, output io.Writer) error {
	return runBrowser(ctx, "command explorer", "Browse commands. Inspect help. Choose what to run.", makeHelpEntries(root), input, output)
}

func commandTUIEnabled(cmd *cobra.Command, interactive bool) bool {
	noTUI, _ := cmd.Root().PersistentFlags().GetBool("no-tui")
	return interactive && !noTUI
}

func runBrowser(ctx context.Context, title, subtitle string, entries []helpEntry, input io.Reader, output io.Writer) error {
	search := textinput.New()
	search.Prompt = "/ "
	search.Placeholder = "filter commands"
	search.CharLimit = 80
	model := helpModel{title: title, subtitle: subtitle, entries: entries, search: search, width: 80, height: 24}
	program := tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(input), tea.WithOutput(output), tea.WithWindowSize(80, 24))
	_, err := program.Run()
	if errors.Is(err, tea.ErrProgramKilled) {
		return nil
	}
	return err
}

func makeHelpEntries(root *cobra.Command) []helpEntry {
	groups := make(map[string]string, len(root.Groups()))
	groupOrder := make(map[string]int, len(root.Groups()))
	for index, group := range root.Groups() {
		groups[group.ID] = group.Title
		groupOrder[group.ID] = index
	}
	entries := make([]helpEntry, 0, len(root.Commands()))
	for _, command := range root.Commands() {
		if command.Hidden || command.Deprecated != "" {
			continue
		}
		group := groups[command.GroupID]
		order, hasGroup := groupOrder[command.GroupID]
		if group == "" {
			group = "Commands"
		}
		if !hasGroup {
			order = len(groupOrder)
		}
		entries = append(entries, helpEntry{
			name:       command.Name(),
			group:      group,
			groupOrder: order,
			short:      command.Short,
			detail:     strings.TrimSpace(command.UsageString()),
		})
	}
	slices.SortFunc(entries, func(a, b helpEntry) int {
		if order := a.groupOrder - b.groupOrder; order != 0 {
			return order
		}
		return strings.Compare(a.name, b.name)
	})
	return entries
}

func (m helpModel) Init() tea.Cmd { return nil }

func (m helpModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.search.SetWidth(max(10, min(m.width-8, 80)))
	case tea.KeyPressMsg:
		key := msg.String()
		if m.search.Focused() {
			if key == "enter" {
				m.search.Blur()
				return m, nil
			}
			if key == "esc" {
				if m.search.Value() == "" {
					m.search.Blur()
				} else {
					m.search.SetValue("")
				}
				m.selected = 0
				return m, nil
			}
			var cmd tea.Cmd
			m.search, cmd = m.search.Update(msg)
			m.selected = min(m.selected, max(0, len(m.filtered())-1))
			m.scroll = 0
			return m, cmd
		}
		switch key {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "esc":
			if m.showHelp {
				m.showHelp = false
				m.scroll = 0
			} else {
				return m, tea.Quit
			}
		case "/":
			m.search.Focus()
		case "up", "k":
			if m.showHelp {
				m.scroll = max(0, m.scroll-1)
			} else {
				m.selected = max(0, m.selected-1)
				m.scroll = 0
			}
		case "down", "j":
			if m.showHelp {
				m.scroll++
			} else {
				m.selected = min(max(0, len(m.filtered())-1), m.selected+1)
				m.scroll = 0
			}
		case "pgup":
			if m.showHelp {
				m.scroll = max(0, m.scroll-max(1, m.height/2))
			}
		case "pgdown", " ":
			if m.showHelp {
				m.scroll += max(1, m.height/2)
			}
		case "home":
			if m.showHelp {
				m.scroll = 0
			} else {
				m.selected = 0
			}
		case "end":
			if m.showHelp {
				entries := m.filtered()
				if len(entries) > 0 {
					m.scroll = len(m.entries[entries[m.selected]].detail)
				}
			} else {
				m.selected = max(0, len(m.filtered())-1)
			}
		case "enter":
			if len(m.filtered()) > 0 {
				m.showHelp = !m.showHelp
				m.scroll = 0
			}
		}
	}
	return m, nil
}

func (m helpModel) View() tea.View {
	view := tea.NewView(m.render())
	view.AltScreen = true
	return view
}

func (m helpModel) render() string {
	if m.width < helpMinimumWidth || m.height < helpMinimumHeight {
		return lipgloss.JoinVertical(lipgloss.Left,
			brandHeader(m.title),
			"",
			warnStyle.Bold(true).Render("Terminal too small"),
			mutedStyle.Render(fmt.Sprintf("Resize to at least %d columns by %d rows.", helpMinimumWidth, helpMinimumHeight)),
			"",
			mutedStyle.Render("q quit"),
		)
	}
	if m.width >= helpWideBreakpoint {
		return m.renderWide()
	}
	if m.showHelp {
		return m.renderDetail(m.width-4, m.height-7, true)
	}
	return m.renderNarrow()
}

func (m helpModel) renderWide() string {
	listWidth := 27
	detailWidth := m.width - listWidth - 7
	panelHeight := m.height - 8
	header := lipgloss.JoinVertical(lipgloss.Left,
		brandHeader(m.title),
		mutedStyle.Render(m.subtitle),
		"",
		m.searchView(),
	)
	list := m.renderList(listWidth, panelHeight)
	detail := m.renderDetail(detailWidth, panelHeight, m.showHelp)
	panels := lipgloss.JoinHorizontal(lipgloss.Top, list, "  ", detail)
	return lipgloss.JoinVertical(lipgloss.Left, header, "", panels, "", m.footer())
}

func (m helpModel) renderNarrow() string {
	header := lipgloss.JoinVertical(lipgloss.Left,
		brandHeader(m.title),
		mutedStyle.Render(m.subtitle),
		"",
		m.searchView(),
	)
	list := m.renderList(m.width-4, m.height-8)
	return lipgloss.JoinVertical(lipgloss.Left, header, "", list, "", m.footer())
}

func (m helpModel) searchView() string {
	if m.search.Focused() || m.search.Value() != "" {
		return m.search.View()
	}
	return mutedStyle.Render("/ filter commands")
}

func (m helpModel) renderList(width, height int) string {
	entries := m.filtered()
	if len(entries) == 0 {
		message := "No items to show."
		if strings.TrimSpace(m.search.Value()) != "" {
			message = "No matching items."
		}
		return lipgloss.NewStyle().Width(width).Render(mutedStyle.Render(message))
	}
	rowHeight := max(1, height)
	start := max(0, min(m.selected-rowHeight/2, len(entries)-rowHeight))
	end := min(len(entries), start+rowHeight)
	lines := make([]string, 0, end-start)
	for position := start; position < end; position++ {
		entry := m.entries[entries[position]]
		name := runewidth.Truncate(entry.name, max(1, width-5), "…")
		line := "  " + name
		if position == m.selected {
			line = tealStyle.Bold(true).Render("> " + name)
		} else {
			line = mutedStyle.Render(line)
		}
		lines = append(lines, line)
	}
	return lipgloss.NewStyle().Width(width).Render(strings.Join(lines, "\n"))
}

func (m helpModel) renderDetail(width, height int, full bool) string {
	entries := m.filtered()
	if len(entries) == 0 {
		return ""
	}
	entry := m.entries[entries[m.selected]]
	if full {
		content := entry.detail
		if content == "" {
			content = entry.short
		}
		content = visibleLines(lipgloss.NewStyle().Width(max(1, width-4)).Render(content), m.scroll, max(1, height-4))
		return lipgloss.NewStyle().Width(width).Padding(1).Border(lipgloss.RoundedBorder()).BorderForeground(colorViolet).Render(content)
	}
	group := mutedStyle.Render(strings.ToUpper(entry.group))
	title := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("255")).Render(entry.name)
	short := lipgloss.NewStyle().Width(max(1, width-4)).Render(entry.short)
	content := lipgloss.JoinVertical(lipgloss.Left, group, title, "", short, "", mutedStyle.Render("Press enter to inspect full help."))
	content = limitLines(content, max(1, height-4))
	return lipgloss.NewStyle().Width(width).Padding(1).Border(lipgloss.RoundedBorder()).BorderForeground(colorViolet).Render(content)
}

func (m helpModel) filtered() []int {
	query := strings.ToLower(strings.TrimSpace(m.search.Value()))
	indices := make([]int, 0, len(m.entries))
	for index, entry := range m.entries {
		if query == "" || strings.Contains(strings.ToLower(entry.name+" "+entry.group+" "+entry.short), query) {
			indices = append(indices, index)
		}
	}
	return indices
}

func helpFooter(hints ...string) string {
	return mutedStyle.Render(strings.Join(hints, "  ·  "))
}

func (m helpModel) footer() string {
	if m.showHelp {
		return helpFooter("↑/↓ scroll", "pgup/pgdn page", "esc back", "q quit")
	}
	return helpFooter("↑/↓ move", "/ filter", "enter inspect", "q quit")
}

func visibleLines(text string, offset, height int) string {
	lines := strings.Split(text, "\n")
	start := min(max(0, offset), max(0, len(lines)-height))
	end := min(len(lines), start+height)
	visible := strings.Join(lines[start:end], "\n")
	if end < len(lines) {
		visible += "\n…"
	}
	return visible
}

func limitLines(text string, height int) string {
	lines := strings.Split(text, "\n")
	if len(lines) <= height {
		return text
	}
	if height <= 1 {
		return "…"
	}
	return strings.Join(lines[:height-1], "\n") + "\n…"
}
