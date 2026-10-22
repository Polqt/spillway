package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/Polqt/spillway/internal/budget"
	"github.com/Polqt/spillway/internal/config"
	"github.com/Polqt/spillway/internal/store"
)

const dashboardWindow = 24 * time.Hour

var dashboardGroups = []string{"provider", "model", "client_app", "day"}

func dashboardCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) int {
	fs := flag.NewFlagSet("dashboard", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath, "path to the config file holding the dashboard key")
	endpoint := fs.String("endpoint", "", "running Spillway base URL (default: server.listen from -config)")
	interval := fs.Duration("interval", 2*time.Second, "refresh interval (1s-1m)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		report(stderr, "spillway: dashboard: unexpected argument %q\n\n%s", fs.Arg(0), usage)
		return 2
	}
	if *interval < time.Second || *interval > time.Minute {
		report(stderr, "spillway: dashboard: -interval must be between 1s and 1m\n")
		return 2
	}
	var base *url.URL
	if *endpoint == "" {
		listen, err := config.LoadListenWithEnv(*configPath, lookupEnv)
		if err != nil {
			report(stderr, "spillway: dashboard: %v\nRun `spillway init` to create a config, or pass -config.\n", err)
			return 1
		}
		if base, err = listenEndpoint(listen); err != nil {
			report(stderr, "spillway: dashboard: %v\n", err)
			return 2
		}
	} else {
		parsed, err := url.Parse(strings.TrimRight(*endpoint, "/"))
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			report(stderr, "spillway: dashboard: -endpoint must be an http or https base URL\n")
			return 2
		}
		base = parsed
	}
	// The dashboard key is a full local key, so it never crosses a network in cleartext.
	if !isLoopbackHost(base.Hostname()) {
		if base.Scheme != "https" {
			report(stderr, "spillway: dashboard: refusing to send the dashboard key over plain http to %s; use an https -endpoint\n", base.Host)
			return 2
		}
		report(stderr, "spillway: dashboard: sending the dashboard key to %s\n", base.Host)
	}
	keys, err := config.LoadLocalKeysWithEnv(*configPath, lookupEnv)
	if err != nil {
		report(stderr, "spillway: dashboard: %v\nRun `spillway init` to create a config, or pass -config.\n", err)
		return 1
	}
	key := ""
	for _, candidate := range keys {
		if candidate.Dashboard {
			key = candidate.Key.Reveal()
			break
		}
	}
	if key == "" {
		report(stderr, "spillway: dashboard: %s has no local key with `dashboard: true`\nRerun `spillway init` with usage history enabled, or mark a local key with `dashboard: true`.\n", *configPath)
		return 1
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &dashboardClient{
		http: &http.Client{
			Timeout:       5 * time.Second,
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		base:     base,
		key:      key,
		endpoint: base.Host,
	}
	if !interactiveTerminal(stdin, stdout) {
		snapshot, err := client.fetch(ctx, dashboardGroups[0], time.Now())
		if err != nil {
			report(stderr, "spillway: dashboard: %v\n", err)
			return 1
		}
		if _, err := fmt.Fprintln(styledWriter(stdout), renderDashboard(snapshot, nil, 0, 100)); err != nil {
			report(stderr, "spillway: dashboard: write output: %v\n", err)
			return 1
		}
		return 0
	}
	model := dashboardModel{ctx: ctx, client: client, interval: *interval, width: 100}
	if _, err := tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(stdin), tea.WithOutput(stdout)).Run(); err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		report(stderr, "spillway: dashboard: %v\n", err)
		return 1
	}
	return 0
}

type dashboardProvider struct {
	Name               string `json:"name"`
	CredentialRequired bool   `json:"credential_required"`
	CredentialPresent  bool   `json:"credential_present"`
	Signals            []struct {
		Model            string     `json:"model"`
		Reason           string     `json:"reason"`
		CooldownUntil    *time.Time `json:"cooldown_until"`
		BreakerState     string     `json:"breaker_state"`
		UpstreamFailures int        `json:"upstream_failures"`
	} `json:"signals"`
}

type dashboardSnapshot struct {
	Endpoint     string
	Version      string
	Providers    []dashboardProvider
	Group        string
	Usage        []store.UsageRow
	UsageEnabled bool
	Budget       budget.Status
	FetchedAt    time.Time
}

// dashboardClient reads the gateway's read-only dashboard API with a dashboard key.
type dashboardClient struct {
	http     *http.Client
	base     *url.URL
	key      string
	endpoint string
}

func (c *dashboardClient) fetch(ctx context.Context, group string, now time.Time) (dashboardSnapshot, error) {
	snapshot := dashboardSnapshot{Endpoint: c.endpoint, Group: group, FetchedAt: now, UsageEnabled: true}
	var gateway struct {
		Version string `json:"version"`
	}
	if err := c.get(ctx, "/api/gateway", nil, &gateway); err != nil {
		return snapshot, err
	}
	snapshot.Version = printable(gateway.Version)
	var providerStatus struct {
		Providers []dashboardProvider `json:"providers"`
	}
	if err := c.get(ctx, "/api/providers", nil, &providerStatus); err != nil {
		return snapshot, err
	}
	snapshot.Providers = providerStatus.Providers
	for i := range snapshot.Providers {
		provider := &snapshot.Providers[i]
		provider.Name = printable(provider.Name)
		for j := range provider.Signals {
			provider.Signals[j].Model = printable(provider.Signals[j].Model)
			provider.Signals[j].Reason = printable(provider.Signals[j].Reason)
		}
	}
	if err := c.get(ctx, "/api/budget", nil, &snapshot.Budget); err != nil {
		return snapshot, err
	}
	query := url.Values{
		"group": {group},
		"since": {now.Add(-dashboardWindow).UTC().Format(time.RFC3339)},
		"until": {now.UTC().Format(time.RFC3339)},
	}
	var usage struct {
		Rows []store.UsageRow `json:"data"`
	}
	err := c.get(ctx, "/api/usage", query, &usage)
	var apiErr dashboardAPIError
	if errors.As(err, &apiErr) && apiErr.code == "store_disabled" {
		snapshot.UsageEnabled = false
		return snapshot, nil
	}
	snapshot.Usage = usage.Rows
	for i := range snapshot.Usage {
		snapshot.Usage[i].Key = printable(snapshot.Usage[i].Key)
	}
	return snapshot, err
}

// printable replaces control and other non-printing runes from the gateway,
// so a foreign endpoint cannot drive the terminal with escape sequences.
func printable(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return '?'
	}, value)
}

// listenEndpoint is the loopback base URL for a server.listen address.
func listenEndpoint(listen string) (*url.URL, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" || port == "0" {
		return nil, fmt.Errorf("cannot derive the gateway address from server.listen %q; pass -endpoint", listen)
	}
	switch strings.Trim(host, "[]") {
	case "", "0.0.0.0", "::", "localhost":
		host = "127.0.0.1"
	}
	return &url.URL{Scheme: "http", Host: net.JoinHostPort(strings.Trim(host, "[]"), port)}, nil
}

type dashboardAPIError struct {
	status  int
	code    string
	message string
}

func (e dashboardAPIError) Error() string {
	if e.status == http.StatusUnauthorized {
		return "the gateway rejected the dashboard key; check that -config matches the running server"
	}
	return fmt.Sprintf("gateway answered %d: %s", e.status, e.message)
}

func (c *dashboardClient) get(ctx context.Context, path string, query url.Values, into any) error {
	target := c.base.JoinPath(path)
	target.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.key)
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("spillway is not answering at %s; start it with `spillway serve`", c.endpoint)
	}
	defer func() { _ = response.Body.Close() }()
	body := io.LimitReader(response.Body, 4<<20)
	if response.StatusCode != http.StatusOK {
		var envelope struct {
			Error struct {
				Message string `json:"message"`
				Code    string `json:"code"`
			} `json:"error"`
		}
		_ = json.NewDecoder(body).Decode(&envelope)
		return dashboardAPIError{status: response.StatusCode, code: envelope.Error.Code, message: printable(envelope.Error.Message)}
	}
	if err := json.NewDecoder(body).Decode(into); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return nil
}

type dashboardModel struct {
	ctx      context.Context
	client   *dashboardClient
	interval time.Duration
	group    int
	snapshot *dashboardSnapshot
	err      error
	width    int
	// generation tags the one live refresh chain; results and ticks from older chains are dropped.
	generation int
}

type dashboardResultMsg struct {
	generation int
	snapshot   dashboardSnapshot
	err        error
}

type dashboardTickMsg struct{ generation int }

func (m dashboardModel) refresh() tea.Cmd {
	group, generation := dashboardGroups[m.group], m.generation
	return func() tea.Msg {
		snapshot, err := m.client.fetch(m.ctx, group, time.Now())
		return dashboardResultMsg{generation: generation, snapshot: snapshot, err: err}
	}
}

func (m dashboardModel) Init() tea.Cmd { return m.refresh() }

func (m dashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "r":
			m.generation++
			return m, m.refresh()
		case "tab", "g":
			m.group = (m.group + 1) % len(dashboardGroups)
			m.generation++
			return m, m.refresh()
		}
	case dashboardResultMsg:
		if msg.generation != m.generation {
			return m, nil
		}
		m.err = msg.err
		if msg.err == nil {
			m.snapshot = &msg.snapshot
		}
		generation := m.generation
		return m, tea.Tick(m.interval, func(time.Time) tea.Msg { return dashboardTickMsg{generation: generation} })
	case dashboardTickMsg:
		if msg.generation != m.generation {
			return m, nil
		}
		return m, m.refresh()
	}
	return m, nil
}

func (m dashboardModel) View() tea.View {
	var content string
	if m.snapshot == nil {
		content = "\n  " + brandHeader("dashboard") + "\n\n  " + mutedStyle.Render("connecting to "+m.client.endpoint+" …")
		if m.err != nil {
			content += "\n\n  " + errStyle.Render("✘ "+m.err.Error())
		}
	} else {
		content = renderDashboard(*m.snapshot, m.err, m.group, m.width)
	}
	content += "\n\n  " + mutedStyle.Render("tab group · r refresh · q quit")
	view := tea.NewView(content)
	view.AltScreen = true
	return view
}

// renderDashboard draws one snapshot. A non-nil err marks the data stale.
func renderDashboard(s dashboardSnapshot, err error, group, width int) string {
	width = max(width-4, 60)
	var b strings.Builder

	status := okStyle.Render("● live")
	if err != nil {
		status = errStyle.Render("● stale")
	}
	b.WriteString("\n  " + brandHeader("dashboard · "+s.Version) + "\n")
	b.WriteString("  " + status + "  " + mutedStyle.Render(s.Endpoint+" · updated "+s.FetchedAt.Format("15:04:05")) + "\n")
	if err != nil {
		b.WriteString("  " + errStyle.Render("✘ "+err.Error()) + "\n")
	}

	var requests, errorsCount, cacheHits, promptTokens, completionTokens, cost int64
	for _, row := range s.Usage {
		requests += row.Requests
		errorsCount += row.Errors
		cacheHits += row.CacheHits
		promptTokens += row.PromptTokens
		completionTokens += row.CompletionTokens
		cost += row.CostMicroUSD
	}
	card := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorFaint).Padding(0, 2).MarginRight(1)
	stat := func(label, value string, color lipgloss.Style) string {
		return card.Render(mutedStyle.Render(label) + "\n" + color.Bold(true).Render(value))
	}
	errorColor := okStyle
	if errorsCount > 0 {
		errorColor = errStyle
	}
	b.WriteString("\n" + lipgloss.NewStyle().MarginLeft(2).Render(lipgloss.JoinHorizontal(lipgloss.Top,
		stat("requests · 24h", compactNumber(requests), tealStyle),
		stat("errors", compactNumber(errorsCount), errorColor),
		stat("cache hits", compactNumber(cacheHits), tealStyle),
		stat("tokens in / out", compactNumber(promptTokens)+" / "+compactNumber(completionTokens), tealStyle),
		stat("spend", formatDollars(cost), lipgloss.NewStyle().Foreground(colorViolet)),
	)) + "\n")

	b.WriteString("\n  " + sectionTitle("Providers") + "\n")
	if len(s.Providers) == 0 {
		b.WriteString("  " + mutedStyle.Render("No providers configured.") + "\n")
	}
	for _, provider := range s.Providers {
		b.WriteString("  " + providerLine(provider, s.FetchedAt) + "\n")
	}

	tabs := make([]string, 0, len(dashboardGroups))
	for i, name := range dashboardGroups {
		if i == group {
			tabs = append(tabs, lipgloss.NewStyle().Foreground(lipgloss.Color("#0B1020")).Background(colorTeal).Bold(true).Padding(0, 1).Render(name))
			continue
		}
		tabs = append(tabs, mutedStyle.Padding(0, 1).Render(name))
	}
	b.WriteString("\n  " + sectionTitle("Usage") + " " + mutedStyle.Render("last 24h by") + " " + strings.Join(tabs, "") + "\n")
	switch {
	case !s.UsageEnabled:
		b.WriteString("  " + mutedStyle.Render("Usage history is off. Rerun `spillway init` with usage history to record it.") + "\n")
	case len(s.Usage) == 0:
		b.WriteString("  " + mutedStyle.Render("No requests in the last 24 hours.") + "\n")
	default:
		b.WriteString(lipgloss.NewStyle().MarginLeft(2).Render(usageTable(s.Usage, requests, width)) + "\n")
	}

	if periods := budgetLines(s.Budget, width); periods != "" {
		b.WriteString("\n  " + sectionTitle("Budget") + "\n" + periods)
	}
	return strings.TrimRight(b.String(), "\n")
}

func sectionTitle(title string) string {
	return lipgloss.NewStyle().Bold(true).Foreground(colorViolet).Render(title)
}

func providerLine(p dashboardProvider, now time.Time) string {
	name := lipgloss.NewStyle().Bold(true).Width(12).Render(p.Name)
	if p.CredentialRequired && !p.CredentialPresent {
		return warnStyle.Render("●") + " " + name + warnStyle.Render("key missing")
	}
	for _, signal := range p.Signals {
		if signal.BreakerState == "open" {
			return errStyle.Render("●") + " " + name + errStyle.Render("breaker open") + " " + mutedStyle.Render(signal.Model)
		}
	}
	for _, signal := range p.Signals {
		if signal.CooldownUntil != nil && signal.CooldownUntil.After(now) {
			wait := signal.CooldownUntil.Sub(now).Round(time.Second)
			return warnStyle.Render("◐") + " " + name + warnStyle.Render("cooling down "+wait.String()) + " " + mutedStyle.Render(signal.Model+" · "+signal.Reason)
		}
	}
	return okStyle.Render("●") + " " + name + okStyle.Render("ready")
}

func usageTable(rows []store.UsageRow, total int64, width int) string {
	barWidth := max(min(width-78, 24), 6)
	data := make([][]string, 0, len(rows))
	for _, row := range rows {
		share := 0
		if total > 0 {
			share = int(row.Requests * int64(barWidth) / total)
		}
		bar := gradient(strings.Repeat("█", share)) + lipgloss.NewStyle().Foreground(colorFaint).Render(strings.Repeat("░", barWidth-share))
		data = append(data, []string{
			row.Key, bar, compactNumber(row.Requests), compactNumber(row.Errors), compactNumber(row.CacheHits),
			compactNumber(row.PromptTokens) + " / " + compactNumber(row.CompletionTokens), formatDollars(row.CostMicroUSD),
		})
	}
	return table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(colorFaint)).
		BorderColumn(false).
		Headers("KEY", "SHARE", "REQ", "ERR", "CACHE", "TOKENS IN / OUT", "SPEND").
		Rows(data...).
		StyleFunc(func(row, col int) lipgloss.Style {
			style := lipgloss.NewStyle().Padding(0, 1)
			if row == table.HeaderRow {
				return style.Foreground(colorMuted).Bold(true)
			}
			if col >= 2 {
				style = style.Align(lipgloss.Right)
			}
			if col == 3 && rows[row].Errors > 0 {
				style = style.Foreground(colorRed)
			}
			return style
		}).
		String()
}

func budgetLines(status budget.Status, width int) string {
	var b strings.Builder
	for _, period := range []struct {
		name   string
		status *budget.PeriodStatus
	}{{"daily", status.Daily}, {"monthly", status.Monthly}} {
		if period.status == nil || period.status.LimitMicroUSD <= 0 {
			continue
		}
		barWidth := max(min(width-60, 30), 10)
		filled := int(min(period.status.SpentMicroUSD, period.status.LimitMicroUSD) * int64(barWidth) / period.status.LimitMicroUSD)
		color := okStyle
		if filled*10 >= barWidth*8 {
			color = warnStyle
		}
		bar := color.Render(strings.Repeat("█", filled)) + lipgloss.NewStyle().Foreground(colorFaint).Render(strings.Repeat("░", barWidth-filled))
		fmt.Fprintf(&b, "  %s %s %s %s\n",
			lipgloss.NewStyle().Width(8).Render(period.name), bar,
			formatDollars(period.status.SpentMicroUSD)+" / "+formatDollars(period.status.LimitMicroUSD),
			mutedStyle.Render("resets "+period.status.ResetAt.UTC().Format("Jan 2 15:04 UTC")))
	}
	return b.String()
}

func compactNumber(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 10_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprint(n)
	}
}

func formatDollars(micro int64) string {
	return fmt.Sprintf("$%.4f", float64(micro)/1_000_000)
}
