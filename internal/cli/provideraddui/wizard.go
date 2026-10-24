package provideraddui

import (
	"context"
	"fmt"
	"io"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/Polqt/spillway/internal/cli/initui"
)

// Provider describes a provider choice available in the add wizard.
type Provider struct {
	Name               string
	Hint               string
	KeyEnv             string
	SuggestedModel     string
	RequiresKey        bool
	FreeTierAttestable bool
}

// Draft contains the values collected before the provider change is prepared.
type Draft struct {
	ConfigPath        string
	ServiceType       string
	InstanceName      string
	KeyEnv            string
	AttestFreeTier    bool
	Tier              string
	AddSuggestedModel bool
}

// Prepared contains a safe config preview and the confirmed write operation.
type Prepared struct {
	Preview string
	Success string
	Save    func(context.Context) error
}

// PrepareFunc builds the provider config preview after the wizard inputs are complete.
type PrepareFunc func(Draft) (Prepared, error)

// Options configures the provider add wizard.
type Options struct {
	ConfigPath      string
	Providers       []Provider
	InitialService  string
	InstanceName    string
	InitialKeyEnv   string
	InitialAttested bool
	InitialTier     string
	AskProvider     bool
	AskKeyEnv       bool
	AskAttestation  bool
	AskTier         bool
	DryRun          bool
	ValidateKeyEnv  func(string) error
	Prepare         PrepareFunc
}

// Result contains the selected values and success message.
type Result struct {
	Draft   Draft
	Success string
}

type step int

const (
	stepProvider step = iota
	stepKeyEnv
	stepAttestation
	stepTier
	stepReview
	stepDone
)

type model struct {
	ctx      context.Context
	options  Options
	draft    Draft
	steps    []step
	index    int
	choice   int
	width    int
	height   int
	keyInput textinput.Model
	prepared Prepared
	error    string
	done     bool
	canceled bool
}

// Run displays the styled provider add wizard and writes the config only after review.
func Run(ctx context.Context, options Options, input io.Reader, output io.Writer) (Result, error) {
	if options.Prepare == nil {
		return Result{}, fmt.Errorf("provider add wizard requires a config preparer")
	}
	if options.AskProvider && len(options.Providers) == 0 {
		return Result{}, fmt.Errorf("provider add wizard requires at least one provider choice")
	}
	keyInput := textinput.New()
	keyInput.Prompt = "Environment variable: "
	keyInput.Placeholder = "GEMINI_API_KEY"
	keyInput.CharLimit = 128
	draft := Draft{
		ConfigPath: options.ConfigPath, ServiceType: options.InitialService,
		InstanceName: options.InstanceName, KeyEnv: options.InitialKeyEnv,
		AttestFreeTier: options.InitialAttested, Tier: options.InitialTier,
		AddSuggestedModel: options.InitialTier != "",
	}
	m := model{ctx: ctx, options: options, draft: draft, keyInput: keyInput}
	m.rebuildSteps()
	if m.currentStep() == stepReview {
		m.prepareReview()
	}
	if m.currentStep() == stepKeyEnv {
		m.keyInput.SetValue(m.draft.KeyEnv)
		m.keyInput.Focus()
	}
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
		return Result{}, context.Canceled
	}
	return Result{Draft: finished.draft, Success: finished.prepared.Success}, nil
}

func (m *model) rebuildSteps() {
	m.steps = m.steps[:0]
	if m.options.AskProvider && m.draft.ServiceType == "" {
		m.steps = append(m.steps, stepProvider)
	}
	provider := m.selectedProvider()
	if provider.RequiresKey && m.options.AskKeyEnv {
		m.steps = append(m.steps, stepKeyEnv)
	}
	if provider.FreeTierAttestable && m.options.AskAttestation {
		m.steps = append(m.steps, stepAttestation)
	}
	if provider.SuggestedModel != "" && m.options.AskTier {
		m.steps = append(m.steps, stepTier)
	}
	m.steps = append(m.steps, stepReview)
	m.index = min(m.index, len(m.steps)-1)
}

func (m model) selectedProvider() Provider {
	for _, provider := range m.options.Providers {
		if provider.Name == m.draft.ServiceType {
			return provider
		}
	}
	return Provider{}
}

func (m model) currentStep() step {
	if m.index >= len(m.steps) {
		return stepDone
	}
	if m.index < 0 {
		return stepReview
	}
	return m.steps[m.index]
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.keyInput.SetWidth(max(10, m.width-28))
		return m, nil
	case tea.KeyPressMsg:
		m.error = ""
		key := msg.String()
		if m.currentStep() == stepKeyEnv {
			return m.updateKeyInput(msg)
		}
		if key == "ctrl+c" || key == "esc" {
			if m.currentStep() == stepDone {
				m.done = true
				return m, tea.Quit
			} else {
				m.canceled = true
				return m, tea.Quit
			}
		}
		if key == "left" || key == "backspace" {
			m.back()
			return m, nil
		}
		if key == "up" || key == "k" {
			m.moveChoice(-1)
			return m, nil
		}
		if key == "down" || key == "j" {
			m.moveChoice(1)
			return m, nil
		}
		if key == "space" {
			if m.currentStep() == stepAttestation || m.currentStep() == stepTier {
				m.choice = 1 - m.choice
			}
			return m, nil
		}
		if key == "enter" || key == "right" {
			return m.activate()
		}
	}
	return m, nil
}

func (m model) updateKeyInput(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "esc" {
		m.back()
		m.keyInput.Blur()
		return m, nil
	}
	if msg.String() == "enter" {
		value := strings.TrimSpace(m.keyInput.Value())
		if m.options.ValidateKeyEnv != nil {
			if err := m.options.ValidateKeyEnv(value); err != nil {
				m.error = err.Error()
				return m, nil
			}
		}
		m.draft.KeyEnv = value
		m.keyInput.Blur()
		m.advance()
		return m, nil
	}
	var cmd tea.Cmd
	m.keyInput, cmd = m.keyInput.Update(msg)
	return m, cmd
}

func (m *model) moveChoice(delta int) {
	if m.currentStep() == stepProvider {
		m.choice = min(max(0, m.choice+delta), len(m.options.Providers)-1)
		return
	}
	if m.currentStep() == stepAttestation || m.currentStep() == stepTier {
		m.choice = min(max(0, m.choice+delta), 1)
	}
}

func (m *model) activate() (tea.Model, tea.Cmd) {
	switch m.currentStep() {
	case stepProvider:
		if len(m.options.Providers) == 0 {
			m.error = "No provider choices are available."
			return m, nil
		}
		provider := m.options.Providers[m.choice]
		m.draft.ServiceType = provider.Name
		if m.draft.InstanceName != "" && m.draft.InstanceName != provider.Name {
			m.draft.KeyEnv = strings.ToUpper(strings.ReplaceAll(m.draft.InstanceName, "-", "_")) + "_API_KEY"
		} else {
			m.draft.KeyEnv = provider.KeyEnv
		}
		m.index = 0
		m.choice = 0
		m.rebuildSteps()
		if m.currentStep() == stepKeyEnv {
			m.keyInput.SetValue(m.draft.KeyEnv)
			m.keyInput.Focus()
		} else if m.currentStep() == stepAttestation || m.currentStep() == stepTier {
			m.choice = 1
		} else if m.currentStep() == stepReview {
			m.prepareReview()
		}
		return m, nil
	case stepAttestation:
		m.draft.AttestFreeTier = m.choice == 0
		m.advance()
		return m, nil
	case stepTier:
		m.draft.AddSuggestedModel = m.choice == 0
		if m.draft.AddSuggestedModel && m.draft.Tier == "" {
			m.draft.Tier = "free"
		} else if !m.draft.AddSuggestedModel {
			m.draft.Tier = ""
		}
		m.advance()
		return m, nil
	case stepReview:
		if m.prepared.Preview == "" && m.error != "" {
			return m, nil
		}
		if m.prepared.Save != nil {
			if err := m.prepared.Save(m.ctx); err != nil {
				m.error = fmt.Sprintf("Could not update config: %v", err)
				return m, nil
			}
		}
		m.index = len(m.steps)
		return m, nil
	case stepDone:
		m.done = true
		return m, tea.Quit
	}
	return m, nil
}

func (m *model) advance() {
	if m.index+1 < len(m.steps) {
		m.index++
	}
	m.choice = 0
	switch m.currentStep() {
	case stepKeyEnv:
		m.keyInput.SetValue(m.draft.KeyEnv)
		m.keyInput.Focus()
	case stepAttestation:
		m.choice = 1
	case stepTier:
		m.choice = 1
	case stepReview:
		m.prepareReview()
	}
}

func (m *model) prepareReview() {
	prepared, err := m.options.Prepare(m.draft)
	if err != nil {
		m.error = err.Error()
		m.prepared = Prepared{}
		return
	}
	m.prepared = prepared
}

func (m *model) back() {
	if m.currentStep() == stepReview {
		if len(m.steps) > 1 {
			m.index--
			if m.currentStep() == stepKeyEnv {
				m.keyInput.Focus()
			}
		}
		return
	}
	if m.index > 0 {
		m.index--
		if m.currentStep() == stepProvider {
			m.draft.ServiceType = ""
			m.rebuildSteps()
			m.index = 0
		} else if m.currentStep() == stepKeyEnv {
			m.keyInput.Focus()
		}
	}
}

func (m model) View() tea.View {
	if m.width == 0 {
		m.width = 80
	}
	if m.height == 0 {
		m.height = 24
	}
	if m.width < 60 || m.height < 20 {
		message := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#F3F0FF")).Render("SPILLWAY / PROVIDER ADD")
		message += fmt.Sprintf("\n\nTerminal size: %d × %d\nResize to at least 60 columns and 20 rows to continue.\n\nPress Esc to cancel.", m.width, m.height)
		view := tea.NewView(lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, message))
		view.AltScreen = !m.canceled && !m.done
		return view
	}
	styles := initui.NewStyles()
	content := m.renderStep(styles)
	if m.error != "" {
		content += "\n\n" + lipgloss.NewStyle().Foreground(lipgloss.Color("#FF9D9D")).Bold(true).Render("!  ACTION NEEDED")
		content += "\n" + lipgloss.NewStyle().Foreground(lipgloss.Color("#FFD2D2")).Render(m.error)
	}
	stepName := m.stepName()
	current := min(m.index+1, len(m.steps)+1)
	help := m.helpText()
	if m.width < 124 {
		help = m.compactHelpText()
	}
	view := tea.NewView(initui.RenderFrame(initui.Frame{
		Width: m.width, Height: m.height, Current: current, Total: len(m.steps) + 1,
		Step: stepName, Content: content, Summary: m.renderSummary(styles), Help: help, Badge: "PROVIDER ADD",
	}))
	view.AltScreen = !m.canceled && !m.done
	if m.currentStep() == stepKeyEnv {
		view.Cursor = m.keyInput.Cursor()
	}
	return view
}

func (m model) stepName() string {
	switch m.currentStep() {
	case stepProvider:
		return "Provider"
	case stepKeyEnv:
		return "Key variable"
	case stepAttestation:
		return "Eligibility"
	case stepTier:
		return "Model tier"
	case stepDone:
		return "Done"
	default:
		return "Review"
	}
}

func (m model) renderSummary(styles initui.Styles) string {
	var b strings.Builder
	b.WriteString(styles.CyanText.Render("YOUR CHANGE"))
	b.WriteString("\n\n" + styles.Title.Render("Provider"))
	name := m.draft.ServiceType
	if name == "" {
		name = "Choose one"
	}
	b.WriteString("\n" + styles.Selected.Render(name))
	if m.draft.InstanceName != "" && m.draft.InstanceName != name {
		b.WriteString("\n" + styles.MutedText.Render("alias "+m.draft.InstanceName))
	}
	b.WriteString("\n\n" + styles.Title.Render("Key variable"))
	if m.draft.KeyEnv == "" {
		b.WriteString("\n" + styles.MutedText.Render("not needed"))
	} else {
		b.WriteString("\n" + styles.Selected.Render(m.draft.KeyEnv))
	}
	b.WriteString("\n\n" + styles.Title.Render("Zero-spend"))
	if m.draft.AttestFreeTier {
		b.WriteString("\n" + styles.Selected.Render("Attested"))
	} else {
		b.WriteString("\n" + styles.MutedText.Render("Not attested"))
	}
	b.WriteString("\n\n" + styles.Title.Render("Config"))
	b.WriteString("\n" + styles.MutedText.Render(m.draft.ConfigPath))
	b.WriteString("\n\n" + styles.MutedText.Render("Credentials stay in your environment."))
	return b.String()
}

func (m model) renderStep(styles initui.Styles) string {
	var b strings.Builder
	b.WriteString(styles.Title.Render("Add a provider instance"))
	b.WriteString("\n")
	b.WriteString(styles.MutedText.Render("Preview the config change before saving. Provider keys remain in environment variables."))
	b.WriteString("\n\n")
	switch m.currentStep() {
	case stepProvider:
		for i, provider := range m.options.Providers {
			prefix := "  "
			if i == m.choice {
				prefix = styles.Active.Render("› ")
			}
			b.WriteString(prefix + styles.Selected.Render(provider.Name) + "  " + styles.MutedText.Render(provider.Hint) + "\n")
		}
	case stepKeyEnv:
		b.WriteString(styles.MutedText.Render("Enter the environment variable name holding this provider key. Do not paste the key itself."))
		b.WriteString("\n\n" + m.keyInput.View())
	case stepAttestation, stepTier:
		choices := []string{"Yes", "No"}
		if m.currentStep() == stepAttestation {
			b.WriteString(styles.Title.Render("Does this account or project have no paid plan?"))
			b.WriteString("\n" + styles.MutedText.Render("Only attest when this is true. It does not create extra provider quota."))
		} else {
			provider := m.selectedProvider()
			model := m.draft.ServiceType + "/" + provider.SuggestedModel
			b.WriteString(styles.Title.Render("Add the suggested model to the free tier?"))
			b.WriteString("\n" + styles.MutedText.Render("Suggestion: "+model+". Only eligible models are added."))
		}
		b.WriteString("\n\n")
		for i, choice := range choices {
			prefix := "  "
			mark := "○"
			if i == m.choice {
				prefix = styles.Active.Render("› ")
				mark = "●"
			}
			b.WriteString(prefix + mark + "  " + choice + "\n")
		}
	case stepReview:
		b.WriteString(styles.Title.Render("Review before saving"))
		b.WriteString("\n" + styles.MutedText.Render("A private backup is created before the config is updated."))
		b.WriteString("\n\n")
		preview := m.prepared.Preview
		lines := strings.Split(preview, "\n")
		limit := max(1, m.height-17)
		if len(lines) > limit {
			lines = append(lines[:limit], "… preview shortened to fit terminal …")
		}
		b.WriteString(strings.Join(lines, "\n"))
		if m.options.DryRun {
			b.WriteString("\n\n" + styles.Selected.Render("Dry run only. No config changes will be written."))
		} else {
			b.WriteString("\n\n" + styles.Selected.Render("Apply this change"))
		}
	case stepDone:
		b.WriteString(styles.Title.Render("Provider setup complete"))
		b.WriteString("\n\n" + m.prepared.Success)
		b.WriteString("\n\n" + styles.Selected.Render("Press Enter to finish"))
	}
	return b.String()
}

func (m model) helpText() string {
	switch m.currentStep() {
	case stepProvider:
		return "↑/↓ choose provider   Enter select   Esc cancel"
	case stepKeyEnv:
		return "Type variable name   Enter continue   ← back   Esc cancel"
	case stepReview:
		return "Enter confirm   ← back   Esc cancel"
	case stepDone:
		return "Enter finish"
	default:
		return "↑/↓ choose   Enter confirm   ← back   Esc cancel"
	}
}

func (m model) compactHelpText() string {
	switch m.currentStep() {
	case stepProvider:
		return "↑↓ choose  Enter next  Esc cancel"
	case stepKeyEnv:
		return "Type variable  Enter next  ← back"
	case stepReview:
		return "Enter apply  ← back  Esc cancel"
	case stepDone:
		return "Enter finish"
	default:
		return "↑↓ choose  Enter next  ← back"
	}
}
