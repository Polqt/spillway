package initui

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type Provider struct {
	Name               string
	Hint               string
	SuggestedModel     string
	FreeTierAttestable bool
}

type Draft struct {
	ConfigPath string
	Providers  []string
	Attested   []string
	Models     []string
	Store      bool
}

type Prepared struct {
	Preview string
	Save    func(context.Context) error
	Success string
}

type PrepareFunc func(Draft) (Prepared, error)

type Options struct {
	ConfigPath string
	Providers  []Provider
	Prepare    PrepareFunc
}

type Result struct {
	Draft   Draft
	Success string
}

type step int

const (
	stepPath step = iota
	stepProviders
	stepAttestations
	stepModels
	stepStore
	stepReview
	stepSuccess
)

type model struct {
	ctx           context.Context
	options       Options
	draft         Draft
	step          step
	width         int
	height        int
	cursor        int
	search        textinput.Model
	prepared      Prepared
	error         string
	modelsVisited bool
	done          bool
	canceled      bool
}

func Run(ctx context.Context, options Options, input io.Reader, output io.Writer) (Result, error) {
	if options.Prepare == nil {
		return Result{}, fmt.Errorf("init wizard requires a config preparer")
	}
	if len(options.Providers) == 0 {
		return Result{}, fmt.Errorf("init wizard requires at least one provider option")
	}
	search := textinput.New()
	search.Placeholder = "Filter models..."
	search.Prompt = "/ "
	search.SetValue(options.ConfigPath)
	search.Focus()
	draft := Draft{ConfigPath: options.ConfigPath, Providers: []string{"mock"}}
	m := model{ctx: ctx, options: options, draft: draft, step: stepPath, search: search}
	program := tea.NewProgram(m, tea.WithInput(input), tea.WithOutput(output))
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			program.Quit()
		case <-done:
		}
	}()
	last, err := program.Run()
	if err != nil {
		return Result{}, err
	}
	finished, ok := last.(model)
	if !ok || !finished.done {
		if finished.canceled {
			return Result{}, context.Canceled
		}
		return Result{}, context.Canceled
	}
	return Result{Draft: finished.draft, Success: finished.prepared.Success}, nil
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.search.SetWidth(max(10, m.width-22))
		return m, nil
	case tea.KeyPressMsg:
		m.error = ""
		key := msg.String()
		if key == "ctrl+c" || key == "esc" {
			if m.step == stepSuccess {
				m.done = true
			} else {
				m.canceled = true
				m.step = stepSuccess
			}
			return m, tea.Quit
		}
		if m.step == stepPath {
			return m.updatePath(msg)
		}
		if m.step == stepModels && m.search.Focused() && key != "enter" && key != "up" && key != "down" && key != "tab" {
			var cmd tea.Cmd
			m.search, cmd = m.search.Update(msg)
			m.cursor = min(m.cursor, len(m.filteredModels()))
			return m, cmd
		}
		if key == "left" || key == "backspace" {
			m.back()
			return m, nil
		}
		if key == "up" || key == "k" {
			m.cursor = max(0, m.cursor-1)
			return m, nil
		}
		if key == "down" || key == "j" {
			m.cursor = min(m.cursor+1, m.itemCount())
			return m, nil
		}
		if key == "tab" && m.step == stepModels {
			m.search.Blur()
			return m, nil
		}
		if key == "enter" || key == "right" {
			if m.step == stepReview {
				return m.save()
			}
			if m.step == stepSuccess {
				m.done = true
				return m, tea.Quit
			}
			m.activate()
			return m, nil
		}
		if key == "space" {
			m.toggle()
			return m, nil
		}
	}
	return m, nil
}

func (m model) updatePath(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "enter" {
		m.draft.ConfigPath = strings.TrimSpace(m.search.Value())
		if m.draft.ConfigPath == "" {
			m.error = "Choose a config file path to continue."
			return m, nil
		}
		m.step = stepProviders
		m.cursor = 0
		return m, nil
	}
	var cmd tea.Cmd
	m.search, cmd = m.search.Update(msg)
	return m, cmd
}

func (m *model) activate() {
	switch m.step {
	case stepProviders:
		if m.cursor < len(m.options.Providers) {
			name := m.options.Providers[m.cursor].Name
			m.draft.Providers = toggle(m.draft.Providers, name)
			return
		}
		if len(m.draft.Providers) == 0 {
			m.error = "Select at least one provider."
			return
		}
		m.step, m.cursor = stepAttestations, 0
	case stepAttestations:
		items := m.attestable()
		if m.cursor < len(items) {
			m.draft.Attested = toggle(m.draft.Attested, items[m.cursor].Name)
			return
		}
		m.setModels()
		m.advanceAfterAttestations()
	case stepModels:
		items := m.filteredModels()
		if m.cursor < len(items) {
			m.draft.Models = toggle(m.draft.Models, items[m.cursor])
			return
		}
		if len(m.draft.Models) == 0 {
			m.error = "Select at least one suggested model, or go back and attest eligible providers."
			return
		}
		m.step, m.cursor = stepStore, 0
	case stepStore:
		if m.cursor == 0 {
			m.draft.Store = !m.draft.Store
			return
		}
		m.step, m.cursor = stepReview, 0
		m.prepare()
	}
}

func (m *model) advanceAfterAttestations() {
	if len(m.draft.Models) > 0 {
		m.step = stepModels
		if !m.modelsVisited {
			m.search.SetValue("")
			m.modelsVisited = true
		}
		m.search.Focus()
		return
	}
	m.step = stepStore
}

func (m *model) setModels() {
	eligible := make(map[string]bool, len(m.options.Providers))
	for _, provider := range m.options.Providers {
		if !slices.Contains(m.draft.Providers, provider.Name) || provider.SuggestedModel == "" {
			continue
		}
		if provider.FreeTierAttestable && !slices.Contains(m.draft.Attested, provider.Name) {
			continue
		}
		modelID := provider.Name + "/" + provider.SuggestedModel
		eligible[modelID] = true
	}
	models := make([]string, 0, len(eligible))
	for _, modelID := range m.draft.Models {
		if eligible[modelID] {
			models = append(models, modelID)
		}
	}
	for modelID := range eligible {
		if !slices.Contains(models, modelID) {
			models = append(models, modelID)
		}
	}
	slices.Sort(models)
	m.draft.Models = models
}

func (m model) save() (tea.Model, tea.Cmd) {
	if m.prepared.Save == nil {
		m.error = "The config could not be prepared. Go back and check your selections."
		return m, nil
	}
	if err := m.prepared.Save(m.ctx); err != nil {
		m.error = fmt.Sprintf("Could not save config: %v", err)
		return m, nil
	}
	m.step = stepSuccess
	m.cursor = 0
	return m, nil
}

func (m *model) prepare() {
	prepared, err := m.options.Prepare(m.draft)
	if err != nil {
		m.error = err.Error()
		m.prepared = Prepared{}
		return
	}
	m.prepared = prepared
}

func (m *model) back() {
	switch m.step {
	case stepProviders:
		m.step = stepPath
	case stepAttestations:
		m.step = stepProviders
	case stepModels:
		m.step = stepAttestations
		m.search.Blur()
	case stepStore:
		if len(m.draft.Models) > 0 {
			m.step = stepModels
			m.search.Focus()
		} else {
			m.step = stepAttestations
		}
	case stepReview:
		m.step = stepStore
	case stepSuccess:
		m.done = true
	}
	m.cursor = 0
}

func (m model) toggle() {
	if m.cursor >= m.itemCount() {
		return
	}
	switch m.step {
	case stepProviders:
		if m.cursor < len(m.options.Providers) {
			name := m.options.Providers[m.cursor].Name
			m.draft.Providers = toggle(m.draft.Providers, name)
		}
	case stepAttestations:
		items := m.attestable()
		if m.cursor < len(items) {
			m.draft.Attested = toggle(m.draft.Attested, items[m.cursor].Name)
		}
	case stepModels:
		items := m.filteredModels()
		if m.cursor < len(items) {
			m.draft.Models = toggle(m.draft.Models, items[m.cursor])
		}
	case stepStore:
		m.draft.Store = !m.draft.Store
	}
}

func (m model) itemCount() int {
	switch m.step {
	case stepProviders:
		return len(m.options.Providers)
	case stepAttestations:
		return len(m.attestable())
	case stepModels:
		return len(m.filteredModels())
	case stepStore:
		return 1
	}
	return 0
}

func (m model) attestable() []Provider {
	var items []Provider
	for _, provider := range m.options.Providers {
		if provider.FreeTierAttestable && slices.Contains(m.draft.Providers, provider.Name) {
			items = append(items, provider)
		}
	}
	return items
}

func (m model) filteredModels() []string {
	query := strings.ToLower(strings.TrimSpace(m.search.Value()))
	var items []string
	for _, item := range m.draft.Models {
		if query == "" || strings.Contains(strings.ToLower(item), query) {
			items = append(items, item)
		}
	}
	return items
}

func toggle(values []string, value string) []string {
	if slices.Contains(values, value) {
		return slices.Delete(slices.Clone(values), slices.Index(values, value), slices.Index(values, value)+1)
	}
	return append(values, value)
}

func (m model) View() tea.View {
	if m.width == 0 {
		m.width = 80
	}
	if m.height == 0 {
		m.height = 24
	}
	if m.step == stepSuccess && m.canceled {
		return tea.NewView("Setup cancelled. No config file was created.\n")
	}
	if m.width < 60 || m.height < 18 {
		view := fmt.Sprintf("SPILLWAY / SETUP\n\nTerminal size: %d × %d\nResize to at least 60 × 18 to continue.\n\nEsc cancels setup.", m.width, m.height)
		t := tea.NewView(lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, view))
		t.AltScreen = !m.canceled && !m.done
		return t
	}
	accent := lipgloss.Color("#9B8CFF")
	muted := lipgloss.Color("#9AA4B2")
	title := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#F3F0FF"))
	active := lipgloss.NewStyle().Foreground(accent).Bold(true)
	selected := lipgloss.NewStyle().Foreground(lipgloss.Color("#73E2B7"))
	mutedStyle := lipgloss.NewStyle().Foreground(muted)
	var body strings.Builder
	body.WriteString(title.Render("SPILLWAY"))
	body.WriteString(mutedStyle.Render("   /   LOCAL GATEWAY SETUP"))
	body.WriteString("\n\n")
	steps := []struct {
		id   step
		name string
	}{{stepPath, "Path"}, {stepProviders, "Providers"}, {stepAttestations, "Access"}, {stepModels, "Models"}, {stepStore, "History"}, {stepReview, "Review"}, {stepSuccess, "Done"}}
	if m.width < 104 {
		current, total := 0, 0
		label := "SETUP"
		for _, item := range steps {
			if item.id == stepModels && len(m.draft.Models) == 0 && m.step != stepModels {
				continue
			}
			total++
			if item.id == m.step {
				current = total
				label = strings.ToUpper(item.name)
			}
		}
		body.WriteString(active.Render(fmt.Sprintf("STEP %02d / %02d   %s", current, total, label)))
		body.WriteString("\n\n")
	} else {
		visibleIndex := 0
		for _, item := range steps {
			if item.id == stepModels && len(m.draft.Models) == 0 && m.step != stepModels {
				continue
			}
			if visibleIndex > 0 {
				body.WriteString(mutedStyle.Render("  ─  "))
			}
			visibleIndex++
			label := fmt.Sprintf("%02d %s", visibleIndex, item.name)
			if m.step == item.id {
				body.WriteString(active.Render("● " + label))
			} else if m.step > item.id {
				body.WriteString(selected.Render("✓ " + label))
			} else {
				body.WriteString(mutedStyle.Render("○ " + label))
			}
		}
		body.WriteString("\n\n")
	}
	width := max(44, min(m.width-8, 104))
	panel := lipgloss.NewStyle().Width(width).Padding(1, 2).BorderLeft(true).BorderStyle(lipgloss.ThickBorder()).BorderForeground(accent)
	var content strings.Builder
	content.WriteString(m.renderStep(title, active, selected, mutedStyle))
	if m.error != "" {
		content.WriteString("\n\n")
		content.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("#FF8B8B")).Bold(true).Render(m.error))
	}
	body.WriteString(panel.Render(content.String()))
	body.WriteString("\n\n")
	body.WriteString(mutedStyle.Render(m.helpText()))
	view := lipgloss.Place(min(m.width, width+8), m.height, lipgloss.Left, lipgloss.Top, body.String())
	v := tea.NewView(view)
	v.AltScreen = !m.canceled && !m.done
	if m.step == stepPath || (m.step == stepModels && m.search.Focused()) {
		v.Cursor = m.search.Cursor()
	}
	return v
}

func (m model) renderStep(title, active, selected, muted lipgloss.Style) string {
	var b strings.Builder
	switch m.step {
	case stepPath:
		b.WriteString(title.Render("Where should we create the config?"))
		b.WriteString("\n")
		b.WriteString(muted.Render("Existing files are never overwritten."))
		b.WriteString("\n\n")
		if !m.search.Focused() {
			m.search.Focus()
		}
		b.WriteString(m.search.View())
	case stepProviders:
		b.WriteString(title.Render("Choose your providers"))
		b.WriteString("\n")
		b.WriteString(muted.Render("Select one or more. Mock is ready for an offline first run."))
		b.WriteString("\n\n")
		for i, item := range m.options.Providers {
			prefix := "  "
			if i == m.cursor {
				prefix = active.Render("› ")
			}
			mark := "○"
			if slices.Contains(m.draft.Providers, item.Name) {
				mark = "●"
			}
			line := prefix + mark + "  " + item.Name + "  " + muted.Render(item.Hint)
			b.WriteString(line + "\n")
		}
		b.WriteString("\n" + m.continueLabel(active, selected, len(m.options.Providers)))
	case stepAttestations:
		b.WriteString(title.Render("Confirm free-tier eligibility"))
		b.WriteString("\n")
		b.WriteString(muted.Render("Only check providers whose account or project has no paid plan."))
		b.WriteString("\n\n")
		items := m.attestable()
		if len(items) == 0 {
			b.WriteString(muted.Render("No attestations are needed for these providers."))
		} else {
			for i, item := range items {
				prefix, mark := "  ", "○"
				if i == m.cursor {
					prefix = active.Render("› ")
				}
				if slices.Contains(m.draft.Attested, item.Name) {
					mark = "●"
				}
				b.WriteString(prefix + mark + "  " + item.Name + " has no paid plan\n")
			}
		}
		b.WriteString("\n" + m.continueLabel(active, selected, len(items)))
	case stepModels:
		b.WriteString(title.Render("Choose models for auto"))
		b.WriteString("\n")
		b.WriteString(muted.Render("Local suggestions only. Search filters this list; no provider calls are made."))
		b.WriteString("\n\n" + m.search.View() + "\n\n")
		items := m.filteredModels()
		for i, provider := range m.draft.Providers {
			group := make([]string, 0, 1)
			for _, item := range items {
				if strings.HasPrefix(item, provider+"/") {
					group = append(group, item)
				}
			}
			if len(group) == 0 {
				continue
			}
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(active.Render(strings.ToUpper(provider)) + "\n")
			for _, item := range group {
				position := slices.Index(items, item)
				prefix, mark := "  ", "○"
				if position == m.cursor {
					prefix = active.Render("› ")
				}
				if slices.Contains(m.draft.Models, item) {
					mark = "●"
				}
				b.WriteString(prefix + mark + "  " + strings.TrimPrefix(item, provider+"/") + "\n")
			}
		}
		b.WriteString("\n" + m.continueLabel(active, selected, len(items)))
	case stepStore:
		b.WriteString(title.Render("Local usage history"))
		b.WriteString("\n")
		b.WriteString(muted.Render("Usage metadata stays in a local SQLite database."))
		b.WriteString("\n\n")
		mark := "○"
		if m.draft.Store {
			mark = "●"
		}
		_, _ = fmt.Fprintf(&b, "%s %s  Enable history and dashboard access\n\n", mark, map[bool]string{true: "›", false: " "}[m.cursor == 0])
		b.WriteString(m.continueLabel(active, selected, 1))
	case stepReview:
		b.WriteString(title.Render("Review before saving"))
		b.WriteString("\n")
		b.WriteString(muted.Render("The generated local API key is hidden. Provider credentials stay in environment variables."))
		b.WriteString("\n\n")
		preview := m.prepared.Preview
		if preview == "" {
			preview = "Config preview is not available. Go back, review your choices, and try again."
		}
		limit := max(1, m.height-17)
		lines := strings.Split(preview, "\n")
		if len(lines) > limit {
			lines = append(lines[:limit], "… preview shortened to fit terminal …")
		}
		b.WriteString(strings.Join(lines, "\n"))
		b.WriteString("\n\n" + selected.Render("Save config"))
	case stepSuccess:
		b.WriteString(title.Render("Setup complete"))
		b.WriteString("\n\n")
		b.WriteString(m.prepared.Success)
		b.WriteString("\n\n" + selected.Render("Press Enter to finish"))
	}
	return b.String()
}

func (m model) helpText() string {
	if m.step == stepReview {
		return "↑/↓ navigate   enter save   ← go back   esc cancel"
	}
	if m.step == stepPath {
		return "type path   enter continue   esc cancel"
	}
	if m.step == stepSuccess {
		return "enter finish"
	}
	return "↑/↓ move   space select   enter continue   ← back   esc cancel"
}

func (m model) continueLabel(active, selected lipgloss.Style, cursor int) string {
	if m.cursor == cursor {
		return active.Render("› Continue →")
	}
	return selected.Render("Continue →")
}
