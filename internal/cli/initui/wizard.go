package initui

import (
	"context"
	"fmt"
	"image/color"
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
	ConfigPath    string
	Providers     []string
	GeminiAliases []string
	Attested      []string
	Models        []string
	Store         bool
}

type Prepared struct {
	Preview       string
	Save          func(context.Context) error
	Success       string
	WillOverwrite bool
}

type PrepareFunc func(Draft) (Prepared, error)

type Options struct {
	ConfigPath          string
	Providers           []Provider
	ValidatePath        func(string) error
	ValidateGeminiAlias func(string) error
	Prepare             PrepareFunc
}

type Result struct {
	Draft   Draft
	Success string
}

// Styles contains the colors and text treatments used by interactive commands.
type Styles struct {
	Accent    color.Color
	Cyan      color.Color
	Green     color.Color
	Muted     color.Color
	Title     lipgloss.Style
	Active    lipgloss.Style
	Selected  lipgloss.Style
	MutedText lipgloss.Style
	CyanText  lipgloss.Style
}

// Frame describes a screen rendered with Spillway's interactive terminal design.
type Frame struct {
	Width, Height  int
	Current, Total int
	Step           string
	Content        string
	Summary        string
	Help           string
	Badge          string
}

// NewStyles returns the shared Spillway interactive color palette.
func NewStyles() Styles {
	accent := lipgloss.Color("#8B5CF6")
	cyan := lipgloss.Color("#2DD4BF")
	green := lipgloss.Color("#34D399")
	muted := lipgloss.Color("#A7B0C0")
	return Styles{
		Accent: accent, Cyan: cyan, Green: green, Muted: muted,
		Title:     lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#F4F1FF")),
		Active:    lipgloss.NewStyle().Foreground(accent).Bold(true),
		Selected:  lipgloss.NewStyle().Foreground(green).Bold(true),
		MutedText: lipgloss.NewStyle().Foreground(muted),
		CyanText:  lipgloss.NewStyle().Foreground(cyan).Bold(true),
	}
}

// RenderFrame places command content in the shared setup header, progress bar,
// summary panel and keyboard bar.
func RenderFrame(frame Frame) string {
	width := max(1, frame.Width)
	if frame.Height == 0 {
		frame.Height = 24
	}
	styles := NewStyles()
	muted := styles.MutedText
	barWidth := min(32, max(12, (width-34)/3))
	current := max(1, frame.Current)
	total := max(current, frame.Total)
	filled := (barWidth*current + total - 1) / total
	progress := styles.Active.Render(strings.Repeat("━", filled)) + muted.Render(strings.Repeat("─", max(0, barWidth-filled)))
	progress += fmt.Sprintf("  %02d / %02d  %s", current, total, strings.ToUpper(frame.Step))

	logo := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#F4F1FF")).Render("∿ spillway")
	tagline := muted.Render("Your providers. Your machine.")
	if frame.Badge == "" {
		frame.Badge = "LOCAL · PRIVATE"
	}
	badge := lipgloss.NewStyle().Bold(true).Foreground(styles.Cyan).Render(strings.ToUpper(frame.Badge))
	headerRow := lipgloss.JoinHorizontal(lipgloss.Center, logo, "  ", tagline, "  ", badge)
	if width < 88 {
		headerRow = lipgloss.JoinVertical(lipgloss.Left,
			lipgloss.JoinHorizontal(lipgloss.Center, logo, "  ", badge),
			tagline,
		)
	}
	headerWidth := min(width-8, 124)
	header := lipgloss.NewStyle().Width(headerWidth).Padding(1, 2).BorderBottom(true).BorderForeground(lipgloss.Color("#303345")).Render(headerRow)
	progressLine := lipgloss.NewStyle().Width(headerWidth).Padding(0, 2).Render(progress)

	var layout string
	if width >= 112 {
		mainWidth := min(78, width-48)
		mainPanel := lipgloss.NewStyle().Width(mainWidth).Padding(1, 2).BorderLeft(true).BorderForeground(styles.Accent)
		sidePanel := lipgloss.NewStyle().Width(27).Padding(1, 1)
		layout = lipgloss.JoinHorizontal(lipgloss.Top, mainPanel.Render(frame.Content), "  ", sidePanel.Render(frame.Summary))
	} else {
		mainWidth := min(92, width-10)
		mainPanel := lipgloss.NewStyle().Width(mainWidth).Padding(1, 2).BorderLeft(true).BorderForeground(styles.Accent)
		layout = mainPanel.Render(frame.Content)
	}
	help := lipgloss.NewStyle().Foreground(lipgloss.Color("#D9D4EE")).Padding(0, 1).Render(frame.Help)
	body := lipgloss.JoinVertical(lipgloss.Left, header, progressLine, layout, help)
	return lipgloss.Place(width, frame.Height, lipgloss.Center, lipgloss.Top, body)
}

// GeminiAliasKeyEnv returns the environment variable name for a Gemini alias.
func GeminiAliasKeyEnv(alias string) string {
	return "GEMINI_" + strings.ToUpper(strings.ReplaceAll(alias, "-", "_")) + "_API_KEY"
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
	aliasInput    textinput.Model
	editingAlias  bool
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
	aliasInput := textinput.New()
	aliasInput.Placeholder = "work"
	aliasInput.Prompt = "Project alias: "
	aliasInput.CharLimit = 32
	draft := Draft{ConfigPath: options.ConfigPath}
	m := model{ctx: ctx, options: options, draft: draft, step: stepPath, search: search, aliasInput: aliasInput}
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
		m.aliasInput.SetWidth(max(10, m.width-22))
		return m, nil
	case tea.KeyPressMsg:
		m.error = ""
		key := msg.String()
		if m.step == stepProviders && m.editingAlias {
			return m.updateGeminiAlias(msg)
		}
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
			if m.search.Focused() {
				m.search.Blur()
			} else {
				m.search.Focus()
			}
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
		path := strings.TrimSpace(m.search.Value())
		if m.options.ValidatePath != nil {
			if err := m.options.ValidatePath(path); err != nil {
				m.error = err.Error()
				return m, nil
			}
		}
		if path == "" {
			m.error = "Choose a config file path to continue."
			return m, nil
		}
		m.draft.ConfigPath = path
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
			if !m.hasGeminiAliasAction() {
				m.draft.GeminiAliases = nil
			}
			return
		}
		if m.hasGeminiAliasAction() && m.cursor == m.geminiAliasActionIndex() {
			m.editingAlias = true
			m.aliasInput.SetValue("")
			m.aliasInput.Focus()
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

func (m model) hasGeminiAliasAction() bool {
	return slices.Contains(m.draft.Providers, "gemini")
}

func (m model) geminiAliasActionIndex() int {
	return len(m.options.Providers)
}

func (m model) providerContinueIndex() int {
	index := len(m.options.Providers)
	if m.hasGeminiAliasAction() {
		index++
	}
	return index
}

func (m model) updateGeminiAlias(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "left":
		m.editingAlias = false
		m.aliasInput.Blur()
		return m, nil
	case "enter":
		alias := strings.TrimSpace(m.aliasInput.Value())
		if alias == "" {
			m.error = "Enter a short alias for the Gemini project."
			return m, nil
		}
		if m.options.ValidateGeminiAlias != nil {
			if err := m.options.ValidateGeminiAlias(alias); err != nil {
				m.error = err.Error()
				return m, nil
			}
		}
		for _, provider := range m.options.Providers {
			if alias == provider.Name {
				m.error = "That alias is reserved for a provider name. Choose another alias."
				return m, nil
			}
		}
		if slices.Contains(m.draft.GeminiAliases, alias) {
			m.error = "That Gemini project alias is already in use. Choose another alias."
			return m, nil
		}
		m.draft.GeminiAliases = append(m.draft.GeminiAliases, alias)
		m.editingAlias = false
		m.aliasInput.Blur()
		m.cursor = m.geminiAliasActionIndex()
		return m, nil
	}
	var cmd tea.Cmd
	m.aliasInput, cmd = m.aliasInput.Update(msg)
	return m, cmd
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

func (m *model) toggle() {
	if m.cursor >= m.itemCount() {
		return
	}
	switch m.step {
	case stepProviders:
		if m.cursor < len(m.options.Providers) {
			name := m.options.Providers[m.cursor].Name
			m.draft.Providers = toggle(m.draft.Providers, name)
			if !m.hasGeminiAliasAction() {
				m.draft.GeminiAliases = nil
			}
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
		count := len(m.options.Providers)
		if m.hasGeminiAliasAction() {
			count++
		}
		return count
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
	if m.width < 60 || m.height < 20 {
		view := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#F3F0FF")).Render("SPILLWAY SETUP")
		view += fmt.Sprintf("\n\nTerminal size: %d × %d\nResize to at least 60 columns and 20 rows to continue.\n\nPress Esc to cancel.", m.width, m.height)
		t := tea.NewView(lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, view))
		t.AltScreen = !m.canceled && !m.done
		return t
	}
	styles := NewStyles()

	steps := []struct {
		id   step
		name string
	}{{stepPath, "Config"}, {stepProviders, "Providers"}, {stepAttestations, "Eligibility"}, {stepModels, "Models"}, {stepStore, "History"}, {stepReview, "Review"}, {stepSuccess, "Done"}}
	current, total, currentName := 0, 0, "Setup"
	for _, item := range steps {
		if item.id == stepModels && len(m.draft.Models) == 0 && m.step != stepModels {
			continue
		}
		total++
		if item.id == m.step {
			current, currentName = total, item.name
		}
	}
	if current == 0 {
		current = 1
	}
	content := m.renderStep(styles.Title, styles.Active, styles.Selected, styles.MutedText)
	if m.error != "" {
		errorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#FF9D9D")).Bold(true)
		errorDetail := lipgloss.NewStyle().Foreground(lipgloss.Color("#FFD2D2"))
		content += "\n\n" + errorStyle.Render("!  ACTION NEEDED") + "\n" + errorDetail.Render(m.error)
	}
	helpText := m.helpText()
	if m.width < 124 {
		helpText = m.compactHelpText()
	}
	view := RenderFrame(Frame{
		Width: m.width, Height: m.height, Current: current, Total: total, Step: currentName,
		Content: content,
		Summary: m.renderSummary(styles.CyanText, styles.Title, styles.MutedText, styles.Selected),
		Help:    helpText,
		Badge:   "LOCAL SETUP",
	})
	v := tea.NewView(view)
	v.AltScreen = !m.canceled && !m.done
	if m.step == stepProviders && m.editingAlias {
		v.Cursor = m.aliasInput.Cursor()
	} else if m.step == stepPath || (m.step == stepModels && m.search.Focused()) {
		v.Cursor = m.search.Cursor()
	}
	return v
}

func (m model) renderSummary(section, title, muted, selected lipgloss.Style) string {
	var b strings.Builder
	b.WriteString(section.Render("YOUR SETUP"))
	b.WriteString("\n\n")
	b.WriteString(title.Render("Providers"))
	_, _ = fmt.Fprintf(&b, "\n%d selected", len(m.draft.Providers))
	if len(m.draft.Providers) == 0 {
		b.WriteString("\n" + muted.Render("Choose at least one"))
	} else {
		for _, name := range m.draft.Providers {
			b.WriteString("\n" + selected.Render("• "+name))
		}
	}
	if len(m.draft.GeminiAliases) > 0 {
		b.WriteString("\n\n" + title.Render("Direct Gemini projects"))
		for _, alias := range m.draft.GeminiAliases {
			b.WriteString("\n" + selected.Render("• "+alias))
		}
	}
	b.WriteString("\n\n" + title.Render("Auto models"))
	if len(m.draft.Models) == 0 {
		b.WriteString("\n" + muted.Render("None selected yet"))
	} else {
		for _, modelName := range m.draft.Models {
			b.WriteString("\n" + selected.Render("• "+modelName))
		}
	}
	b.WriteString("\n\n" + title.Render("Local history"))
	if m.draft.Store {
		b.WriteString("\n" + selected.Render("Enabled"))
	} else {
		b.WriteString("\n" + muted.Render("Off"))
	}
	b.WriteString("\n\n" + title.Render("Config file") + "\n" + muted.Render(m.draft.ConfigPath))
	b.WriteString("\n\n" + muted.Render("Credentials stay in your environment."))
	return b.String()
}

func (m model) renderStep(title, active, selected, muted lipgloss.Style) string {
	var b strings.Builder
	switch m.step {
	case stepPath:
		b.WriteString(title.Render("Choose a home for your gateway config"))
		b.WriteString("\n")
		b.WriteString(muted.Render("Use a filename such as config.yaml. Existing regular files are backed up before replacement."))
		b.WriteString("\n\n")
		if !m.search.Focused() {
			m.search.Focus()
		}
		b.WriteString(m.search.View())
	case stepProviders:
		b.WriteString(title.Render("Choose your providers"))
		b.WriteString("\n")
		b.WriteString(muted.Render("Select real providers you use. Provider API keys stay in environment variables."))
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
			name := item.Name
			if slices.Contains(m.draft.Providers, item.Name) {
				name = selected.Render(name)
			}
			line := prefix + mark + "  " + name + "  " + muted.Render(item.Hint)
			b.WriteString(line + "\n")
		}
		if m.hasGeminiAliasAction() {
			b.WriteString("\n")
			if m.editingAlias {
				b.WriteString(m.aliasInput.View())
			} else {
				prefix := "  "
				if m.cursor == m.geminiAliasActionIndex() {
					prefix = active.Render("› ")
				}
				action := "+ Add a named Gemini project"
				if len(m.draft.GeminiAliases) > 0 {
					action = "+ Add another Gemini project"
				}
				b.WriteString(prefix + action + "\n")
				for _, alias := range m.draft.GeminiAliases {
					b.WriteString("    " + alias + " → " + GeminiAliasKeyEnv(alias) + " · direct only, may incur charges\n")
				}
				if len(m.draft.GeminiAliases) > 0 {
					b.WriteString(muted.Render("    Direct models are excluded from automatic routing."))
				}
			}
		}
		b.WriteString("\n" + m.continueLabel(active, selected, m.providerContinueIndex()))
	case stepAttestations:
		b.WriteString(title.Render("Confirm account eligibility"))
		b.WriteString("\n")
		b.WriteString(muted.Render("Check a box only when that provider account or project has no paid plan."))
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
		b.WriteString(title.Render("Choose models for automatic routing"))
		b.WriteString("\n")
		b.WriteString(muted.Render("These are local suggestions. Search filters the list; init never contacts providers."))
		b.WriteString("\n\n" + m.search.View() + "\n\n")
		items := m.filteredModels()
		groupsRendered := 0
		for _, provider := range m.draft.Providers {
			group := make([]string, 0, 1)
			for _, item := range items {
				if strings.HasPrefix(item, provider+"/") {
					group = append(group, item)
				}
			}
			if len(group) == 0 {
				continue
			}
			if groupsRendered > 0 {
				b.WriteString("\n")
			}
			groupsRendered++
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
		b.WriteString(title.Render("Choose local history"))
		b.WriteString("\n")
		b.WriteString(muted.Render("Optional request metadata stays in a local SQLite database on this machine."))
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
		if m.prepared.WillOverwrite {
			warning := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FBBF24"))
			b.WriteString("\n\n" + warning.Render("Existing config will be backed up, then replaced."))
		}
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
		b.WriteString("\n\n" + selected.Render("Save this config"))
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
		return "↑/↓ review   Enter save config   ← back   Esc cancel"
	}
	if m.step == stepPath {
		return "Type a path   Enter continue   Esc cancel"
	}
	if m.step == stepProviders && m.editingAlias {
		return "Type a short alias   Enter add   ←/Esc back   Ctrl+C cancel setup"
	}
	if m.step == stepSuccess {
		return "Enter finish"
	}
	if m.step == stepModels {
		return "Type to search   Tab switch search/list   ↑/↓ move   Space/Enter toggle   Enter on Continue   ← back   Esc cancel"
	}
	if m.step == stepStore {
		return "↑/↓ move   Space/Enter toggle   highlight Continue, then Enter   ← back   Esc cancel"
	}
	return "↑/↓ move   Space/Enter toggle   highlight Continue, then Enter   ← back   Esc cancel"
}

func (m model) compactHelpText() string {
	switch m.step {
	case stepReview:
		return "Enter save  ← back  Esc cancel"
	case stepPath:
		return "Type path  Enter next  Esc cancel"
	case stepProviders:
		if m.editingAlias {
			return "Type alias  Enter add  ←/Esc back  Ctrl+C cancel"
		}
		return "↑↓ move  Space toggle provider  Enter choose  ← back  Esc cancel"
	case stepSuccess:
		return "Enter finish"
	case stepModels:
		return "Type search  Tab list  Space select  Enter next  Esc back"
	default:
		return "↑↓ move  Space toggle  Enter next  ← back  Esc cancel"
	}
}

func (m model) continueLabel(active, selected lipgloss.Style, cursor int) string {
	if m.cursor == cursor {
		return active.Render("› Continue →")
	}
	return selected.Render("Continue →")
}
