package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
	"github.com/pinksaucepasta/paperboat/internal/preferences"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var runPreviewConsoleForCommand = runPreviewConsole

var previewInteractiveTerminal = func(command *cobra.Command) bool {
	input, ok := command.InOrStdin().(*os.File)
	output, outOK := command.OutOrStdout().(*os.File)
	return ok && outOK && term.IsTerminal(int(input.Fd())) && term.IsTerminal(int(output.Fd()))
}

type previewConsoleResult struct {
	action, output string
	err            error
}
type previewConsoleStatus struct {
	lease api.PreviewLease
	err   error
}
type previewConsoleEnded struct{ err error }
type previewConsoleTick struct{}
type previewConsoleModel struct {
	ctx           context.Context
	lease         preview.Lease
	target        preview.LeaseTarget
	visibility    string
	domains       []string
	status        string
	pollError     string
	domainStatus  []api.PreviewDomainSummary
	view          viewport.Model
	width, height int
	input         textinput.Model
	prompt        string
	busy          bool
	stopped       bool
	outcome       previewConsoleResult
	perform       func(string, string) tea.Cmd
	poll          func() tea.Msg
	ended         <-chan error
}

func (m previewConsoleModel) Init() tea.Cmd {
	return tea.Batch(m.poll, func() tea.Msg { return previewConsoleEnded{<-m.ended} })
}
func (m previewConsoleModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.width > 0 {
		m.prepareViewport()
	}
	switch v := msg.(type) {
	case tea.WindowSizeMsg:
		if m.view.Width == 0 {
			m.view = viewport.New(max(1, v.Width), max(1, v.Height-1))
		}
		m.width, m.height = max(1, v.Width), max(1, v.Height)
		m.view.Width, m.view.Height = m.width, max(1, m.height-1)
		m.input.Width = max(1, m.width-4)
		return m, nil
	case previewConsoleEnded:
		m.stopped = true
		m.outcome.err = v.err
		return m, tea.Quit
	case previewConsoleStatus:
		if v.err != nil {
			m.pollError = "Status unavailable: " + userFacingError(v.err)
		} else {
			m.lease.State = v.lease.State
			m.pollError = ""
			m.domainStatus = v.lease.Domains
		}
		return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg { return previewConsoleTick{} })
	case previewConsoleTick:
		return m, m.poll
	case previewConsoleResult:
		m.busy = false
		if v.err != nil {
			m.status = userFacingError(v.err)
			return m, nil
		}
		if v.action == "open" {
			m.status = "Opened preview in your browser."
			return m, nil
		}
		m.outcome = v
		return m, tea.Quit
	case tea.KeyMsg:
		key := v.String()
		if key == "ctrl+c" {
			return m, tea.Quit
		}
		if m.busy {
			if key == "esc" {
				return m, tea.Quit
			}
			return m, nil
		}
		if m.prompt != "" {
			if key == "esc" {
				m.prompt = ""
				return m, nil
			}
			if key == "enter" {
				value := strings.TrimSpace(m.input.Value())
				if value == "" {
					return m, nil
				}
				action := m.prompt
				m.prompt = ""
				m.busy = true
				m.status = "Working… Ctrl+C to cancel."
				return m, m.perform(action, value)
			}
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(v)
			return m, cmd
		}
		keys := previewConsoleKeys(m.ctx)
		switch {
		case key == keys["preview_stop"] || key == "esc":
			return m, tea.Quit
		case key == keys["preview_open"]:
			m.busy = true
			return m, m.perform("open", "")
		case key == keys["preview_background"]:
			m.prompt = "background"
			m.input.SetValue("30m")
			m.input.Focus()
			return m, textinput.Blink
		case key == keys["preview_tunnel"]:
			if len(m.domains) > 0 {
				m.status = "Custom domains cannot be moved automatically. Keep this preview, or create a tunnel with a different domain."
				return m, nil
			}
			m.prompt = "tunnel"
			m.input.SetValue("")
			m.input.Focus()
			return m, textinput.Blink
		}
	}
	var cmd tea.Cmd
	m.view, cmd = m.view.Update(msg)
	return m, cmd
}
func (m previewConsoleModel) panelsContent() string {
	expiry := "until you stop it"
	if m.lease.UserDeadline != nil {
		expiry = m.lease.UserDeadline.Local().Format(time.RFC1123)
	}
	ui := preferences.FromContext(m.ctx).TUI
	panels := ui.PreviewPanels
	if panels == nil {
		panels = []string{"target", "access", "expiry", "domains"}
	}
	var b strings.Builder
	for _, panel := range panels {
		switch panel {
		case "target":
			fmt.Fprintf(&b, "  Target      %s://%s\n", m.target.Scheme, m.target.Address)
		case "access":
			fmt.Fprintf(&b, "  Visibility  %s\n", m.visibility)
		case "expiry":
			fmt.Fprintf(&b, "  Expires     %s\n", expiry)
		case "domains":
			for _, domain := range m.domainStatus {
				_ = writePreviewDomainStatus(&b, domain)
			}
			if len(m.domains) > 0 && len(m.domainStatus) == 0 {
				fmt.Fprintf(&b, "  Domains     %s (checking…)\n", strings.Join(m.domains, ", "))
			}
		}
	}

	return previewConsoleText(b.String())
}
func (m previewConsoleModel) content() string {
	ui := preferences.FromContext(m.ctx).TUI
	var b strings.Builder
	fmt.Fprintf(&b, "\n  PAPERBOAT PREVIEW\n\n  %s\n\n  Status      %s\n", m.lease.Endpoint, m.lease.State)
	b.WriteString(m.panelsContent())
	b.WriteByte('\n')
	if m.pollError != "" {
		fmt.Fprintf(&b, "  %s\n\n", m.pollError)
	}
	if m.prompt == "background" {
		fmt.Fprintf(&b, "  Keep running for (maximum 24h): %s\n  Stops when paperboatd stops; this is not a durable tunnel.\n", m.input.View())
	}
	if m.prompt == "tunnel" {
		fmt.Fprintf(&b, "  Tunnel name: %s\n  Creates a durable tunnel with a new URL, then stops this preview.\n", m.input.View())
	}
	if m.status != "" {
		fmt.Fprintf(&b, "  %s\n\n", m.status)
	}
	keys := previewConsoleKeys(m.ctx)
	fmt.Fprintf(&b, "  %s open browser   %s keep in background   %s make tunnel   %s stop\n  Esc / Ctrl+C stops the preview.\n", keys["preview_open"], keys["preview_background"], keys["preview_tunnel"], keys["preview_stop"])
	content := previewConsoleText(b.String())
	if ui.Density == "compact" {
		content = strings.TrimLeft(strings.ReplaceAll(content, "\n\n", "\n"), "\n")
	}
	return content
}
func previewConsoleKeys(ctx context.Context) map[string]string {
	keys := map[string]string{"preview_open": "o", "preview_background": "b", "preview_tunnel": "t", "preview_stop": "s"}
	for action, key := range preferences.FromContext(ctx).TUI.Keys {
		if _, ok := keys[action]; ok && key != "" {
			keys[action] = key
		}
	}
	return keys
}

func (m previewConsoleModel) frame() ([]string, []string) {
	header := []string{"PAPERBOAT PREVIEW", m.lease.Endpoint, "Status  " + m.lease.State}
	footer := []string{}
	if m.pollError != "" {
		footer = append(footer, m.pollError)
	}
	if m.status != "" {
		footer = append(footer, m.status)
	}
	if m.prompt == "background" {
		footer = append(footer, "Keep running for (max 24h): "+m.input.View(), "Daemon-owned until expiry; stops if paperboatd stops.")
	}
	if m.prompt == "tunnel" {
		footer = append(footer, "Tunnel name: "+m.input.View(), "Creates a durable tunnel with a new URL.")
	}
	keys := previewConsoleKeys(m.ctx)
	actions := fmt.Sprintf("%s open · %s background · %s tunnel · %s stop", keys["preview_open"], keys["preview_background"], keys["preview_tunnel"], keys["preview_stop"])
	footer = append(footer, strings.Split(ansi.Hardwrap(actions, max(1, m.width), true), "\n")...)
	footer = append(footer, "Esc/Ctrl+C stop · ↑/↓ scroll · pgup/pgdn page")
	for i := range header {
		header[i] = ansi.Truncate(previewConsoleLine(header[i]), m.width, "…")
	}
	for i := range footer {
		footer[i] = ansi.Truncate(previewConsoleLine(footer[i]), m.width, "…")
	}
	// Tiny terminals prioritize the status and safety control over optional data.
	if len(header)+len(footer) > m.height {
		if m.height <= 1 {
			return nil, []string{ansi.Truncate("Esc/Ctrl+C stop", m.width, "…")}
		}
		if len(footer) >= m.height {
			footer = footer[len(footer)-m.height+1:]
		}
		header = header[:min(len(header), m.height-len(footer))]
	}
	return header, footer
}
func (m *previewConsoleModel) prepareViewport() {
	header, footer := m.frame()
	m.view.Width, m.view.Height = m.width, max(0, m.height-len(header)-len(footer))
	content := m.panelsContent()
	if m.status != "" {
		content += "\n" + previewConsoleText(m.status) + "\n"
	}
	if m.pollError != "" {
		content += "\n" + previewConsoleText(m.pollError) + "\n"
	}
	if preferences.FromContext(m.ctx).TUI.Density != "compact" {
		content = "\n" + content
	}
	m.view.SetContent(ansi.Hardwrap(content, m.width, true))
}
func (m previewConsoleModel) View() string {
	if m.width == 0 {
		return m.content()
	}
	m.prepareViewport()
	header, footer := m.frame()
	if len(header) > 0 && header[0] == "PAPERBOAT PREVIEW" {
		header[0] = selector.TitleStyle(m.ctx).Render(header[0])
	}
	for i := range footer {
		footer[i] = selector.HelpStyle(m.ctx).Render(footer[i])
	}
	lines := append([]string{}, header...)
	if m.view.Height > 0 {
		lines = append(lines, m.view.View())
	}
	lines = append(lines, footer...)
	return strings.Join(lines, "\n")
}

func previewConsoleLine(value string) string {
	return strings.NewReplacer("\n", " ", "\t", " ").Replace(previewConsoleText(value))
}

func previewConsoleText(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, ansi.Strip(value))
}

func runPreviewConsole(command *cobra.Command, lease preview.Lease, target preview.LeaseTarget, visibility string, domains []string, client *api.Client, ended <-chan error, background func(context.Context, time.Duration) (time.Time, error)) (previewConsoleResult, error) {
	ctx, cancel := context.WithCancel(command.Context())
	var workers sync.WaitGroup
	var lastAction previewConsoleResult
	defer func() { cancel(); workers.Wait() }()
	perform := func(action, value string) tea.Cmd {
		// Launch tracked work now; cancellation cannot discard a pending command.
		workers.Add(1)
		resultCh := make(chan previewConsoleResult, 1)
		go func() {
			defer workers.Done()
			result := previewConsoleResult{action: action}
			switch action {
			case "open":
				result.err = openBrowser(lease.Endpoint)
			case "background":
				ttl, err := time.ParseDuration(value)
				if err != nil || ttl <= 0 || ttl > previewMaximumTTL {
					result.err = errors.New("Enter a positive duration up to 24h, for example 30m or 2h.")
					break
				}
				deadline, err := background(ctx, ttl)
				result.err = err
				if err == nil {
					result.output = fmt.Sprintf("Preview continues in paperboatd until %s: %s\nStop with: pb preview stop %s\n", deadline.Local().Format(time.RFC1123), lease.Endpoint, lease.ID)
				}
			case "tunnel":
				result.output, result.err = createTunnelFromPreview(ctx, command, target, visibility, value)
			}
			lastAction = result
			resultCh <- result
		}()
		return func() tea.Msg {
			select {
			case result := <-resultCh:
				return result
			case <-ctx.Done():
				return nil
			}
		}
	}
	poll := func() tea.Msg {
		pollCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		value, err := client.GetPreviewLease(pollCtx, lease.ID)
		return previewConsoleStatus{value, err}
	}
	input := textinput.New()
	input.CharLimit = 63
	model := previewConsoleModel{ctx: ctx, lease: lease, target: target, visibility: visibility, domains: domains, input: input, view: viewport.New(80, 23), width: 80, height: 24, perform: perform, poll: poll, ended: ended}
	result, err := tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(command.InOrStdin()), tea.WithOutput(command.OutOrStdout())).Run()
	cancel()
	workers.Wait()
	if err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		return previewConsoleResult{}, err
	}
	if final, ok := result.(previewConsoleModel); ok {
		// Cancellation cannot hide a durable resource created just before exit.
		if final.outcome.action == "" && (lastAction.action == "tunnel" || final.stopped && lastAction.err != nil) {
			return lastAction, nil
		}
		return final.outcome, nil
	}
	return previewConsoleResult{}, err
}

func createTunnelFromPreview(ctx context.Context, parent *cobra.Command, target preview.LeaseTarget, visibility, name string) (string, error) {
	command := tunnelCreateCommand()
	command.SetContext(ctx)
	command.SetIn(parent.InOrStdin())
	command.SetErr(io.Discard)
	var output bytes.Buffer
	command.SetOut(&output)
	command.Flags().AddFlagSet(parent.InheritedFlags())
	if err := command.Flags().Set("from", target.Scheme+"://"+target.Address); err != nil {
		return "", err
	}
	if visibility == "private" || visibility == "team" {
		if err := command.Flags().Set(visibility, "true"); err != nil {
			return "", err
		}
	}
	// The live preview remains available until the durable operation is ready.
	if err := command.Flags().Set("wait", "true"); err != nil {
		return "", err
	}
	if err := command.RunE(command, []string{name}); err != nil {
		return "", err
	}
	return output.String(), nil
}
