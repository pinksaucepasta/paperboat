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

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/prompt"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/spf13/cobra"
)

func homeItems() []selector.Item {
	return []selector.Item{
		{ID: "machines", Title: "Machines", Description: "Connect, send files, and manage your computers"},
		{ID: "sessions", Title: "Terminal sessions", Description: "Attach, rename, share, or close durable sessions"},
		{ID: "previews", Title: "Previews & tunnels", Description: "Share a local app, keep it in the background, or create a durable tunnel"},
		{ID: "environment-variables", Title: "ENV Injection", Description: "Manage global and per-machine environment variables"},
		{ID: "inbox", Title: "Team Inbox", Description: "Review and approve teammate file requests"},
		{ID: "team", Title: "Teams", Description: "Members, invitations, shared resources, and permissions"},
		{ID: "transfer", Title: "File transfers", Description: "Inspect, wait for, or cancel file deliveries"},
		{ID: "config", Title: "Configuration", Description: "Sync status, CLI settings, and status bar preferences"},
		{ID: "doctor", Title: "Diagnostics", Description: "Check setup, authentication, and connectivity"},
		{ID: "account", Title: "Account", Description: "Sign in, switch accounts, or sign out"},
		{ID: "customize", Title: "Customize", Description: "Make this CLI yours: shortcuts, appearance, keys, and layout"},
		{ID: "commands", Title: "All commands", Description: "Search current commands, inspect options, and run an exact invocation"},
	}
}

func interactiveCanceled(err error) bool {
	return errors.Is(err, selector.ErrCanceled) || errors.Is(err, prompt.ErrCanceled)
}

// Build child invocations with the parent's explicit connection settings. Never
// carry unrelated flags (or credentials) from the command that opened the menu.
func interactiveArgs(parent *cobra.Command, args []string) []string {
	result := make([]string, 0, len(args)+4)
	for _, name := range []string{"config", "server", "no-customization"} {
		if flag := parent.Flags().Lookup(name); flag != nil && flag.Value.String() != "" {
			if name == "no-customization" {
				if flag.Value.String() == "true" {
					result = append(result, "--no-customization")
				}
				continue
			}
			result = append(result, "--"+name, flag.Value.String())
		}
	}
	return append(result, args...)
}

var newInteractiveRootCommand func() *cobra.Command

func init() { newInteractiveRootCommand = newRootCommand }

func actionHomePreviewLaunch(command *cobra.Command) error {
	target, err := prompt.Text(prompt.TextOptions{Title: "Share a local app", Description: "Local port, HTTP URL, or absolute Unix socket path", Placeholder: "3000", Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr(), Validate: func(value string) error { _, err := parsePreviewTarget(value); return err }})
	if err != nil {
		return err
	}
	access, err := chooseHomeAction(command, "Who can access this app?", []selector.Item{
		{ID: "private", Title: "Private", Description: "Requires authenticated access"},
		{ID: "team", Title: "Team", Description: "Requires an explicit team grant"},
		{ID: "public", Title: "Public", Description: "Anyone with the URL can access it; browser traffic terminates TLS at the edge"},
	})
	if err != nil {
		return err
	}
	mode, err := chooseHomeAction(command, "How long should it run?", []selector.Item{
		{ID: "foreground", Title: "While this terminal is open", Description: "Live preview console with stop and background controls"},
		{ID: "background", Title: "Keep in background", Description: "Daemon-owned preview with a bounded lifetime; no reboot restoration"},
		{ID: "durable", Title: "Create a durable tunnel", Description: "Named tunnel managed by the daemon, including after restart"},
	})
	if err != nil {
		return err
	}
	args := []string{"preview", target}
	if mode.ID == "background" {
		ttl, err := prompt.Text(prompt.TextOptions{Title: "Preview lifetime", Description: "A positive duration up to 24h", Initial: "30m", Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr(), Validate: func(value string) error {
			duration, err := time.ParseDuration(value)
			if err != nil || duration <= 0 || duration > previewMaximumTTL {
				return errors.New("enter a duration greater than zero and no longer than 24h")
			}
			return nil
		}})
		if err != nil {
			return err
		}
		args = append(args, "--background", "--ttl", ttl)
	}
	if mode.ID == "durable" {
		name, err := prompt.Text(prompt.TextOptions{Title: "Tunnel name", Description: "A stable name for this app", Placeholder: "my-app", Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr(), Validate: func(value string) error {
			if !validTunnelCLIName(value, 63) {
				return errors.New("use 1–63 letters, numbers, dots, underscores, or hyphens; start with a letter or number")
			}
			return nil
		}})
		if err != nil {
			return err
		}
		parsed, _ := parsePreviewTarget(target)
		args = []string{"tunnel", "create", name, "--from", formatPreviewTarget(parsed)}
	}
	if access.ID != "public" {
		args = append(args, "--"+access.ID)
	}
	if mode.ID == "foreground" {
		return executeInteractiveCommand(command, args)
	}
	return runHomeResult(command, args)
}

func actionHomePreviews(command *cobra.Command) error {
	for {
		choice, err := chooseHomeAction(command, "Previews & tunnels", []selector.Item{
			{ID: "new", Title: "Share a local app", Description: "Guided preview or durable tunnel setup", Action: true},
			{ID: "previews", Title: "Temporary previews", Description: "View URLs, lifetimes, and stop previews"},
			{ID: "tunnels", Title: "Durable tunnels", Description: "Inspect readiness, routes, domains, connectors, and access"},
		})
		if err != nil {
			return err
		}
		switch choice.ID {
		case "new":
			err = actionHomePreviewLaunch(command)
		case "previews":
			err = actionHomePreviewList(command)
		case "tunnels":
			err = actionHomeTunnelList(command)
		}
		if interactiveCanceled(err) {
			continue
		}
		if err != nil {
			if err = showHomeFailure(command, err); err != nil {
				return err
			}
		}
	}
}

func actionHomePreviewList(command *cobra.Command) error {
	client, err := previewClientForCommand(command)
	if err != nil {
		return err
	}
	cursor := ""
	for {
		var page api.PreviewLeasePage
		err := homeLoading(command, "Temporary previews", "Loading previews", func(ctx context.Context) error {
			var err error
			page, err = client.ListPreviewLeases(ctx, cursor, 50)
			return err
		})
		if err != nil {
			return err
		}
		items := make([]selector.Item, 0, len(page.Items)+2)
		for _, lease := range page.Items {
			items = append(items, selector.Item{ID: lease.ID, Title: lease.Endpoint, Description: preferenceDetails(command.Context(), "previews", map[string]string{"state": lease.State, "access": lease.AccessMode, "id": lease.ID})})
		}
		items = append(items, selector.Item{ID: "refresh", Title: "Refresh", Action: true})
		if page.NextCursor != "" {
			items = append(items, selector.Item{ID: "next", Title: "Next page", Action: true})
		}
		choice, err := chooseHomeAction(command, "Temporary previews", items)
		if err != nil {
			return err
		}
		if choice.ID == "refresh" {
			cursor = ""
			continue
		}
		if choice.ID == "next" {
			cursor = page.NextCursor
			continue
		}
		action, err := chooseHomeAction(command, "Preview "+choice.Title, []selector.Item{{ID: "status", Title: "Details", Description: "Current readiness, URL, target, and expiry"}, {ID: "stop", Title: "Stop preview", Description: "End this temporary preview"}})
		if interactiveCanceled(err) {
			continue
		}
		if err != nil {
			return err
		}
		if action.ID == "stop" {
			yes, err := prompt.Confirm(prompt.ConfirmOptions{Title: "Stop this preview?", Description: choice.Title, Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr()})
			if err != nil {
				return err
			}
			if !yes {
				continue
			}
		}
		err = runHomeResult(command, []string{"preview", action.ID, choice.ID})
		if err != nil && !interactiveCanceled(err) {
			return err
		}
	}
}

func actionHomeTunnelList(command *cobra.Command) error {
	client, err := tunnelClientForCommand(command)
	if err != nil {
		return err
	}
	cursor := ""
	for {
		var page api.TunnelPage
		err := homeLoading(command, "Durable tunnels", "Loading tunnels", func(ctx context.Context) error {
			var err error
			page, err = client.ListTunnelsV1(ctx, cursor, 50)
			return err
		})
		if err != nil {
			return err
		}
		items := make([]selector.Item, 0, len(page.Items)+3)
		for _, tunnel := range page.Items {
			items = append(items, selector.Item{ID: tunnel.ID, Title: tunnel.Name, Description: preferenceDetails(command.Context(), "tunnels", map[string]string{"state": tunnel.DesiredState, "access": tunnel.AccessMode, "endpoint": tunnel.StableEndpoint})})
		}
		items = append(items, selector.Item{ID: "refresh", Title: "Refresh", Action: true}, selector.Item{ID: "advanced", Title: "All tunnel commands", Description: "Routes, domains, policies, connectors, credentials, and inspector", Action: true})
		if page.NextCursor != "" {
			items = append(items, selector.Item{ID: "next", Title: "Next page", Action: true})
		}
		choice, err := chooseHomeAction(command, "Durable tunnels", items)
		if err != nil {
			return err
		}
		if choice.ID == "refresh" {
			cursor = ""
			continue
		}
		if choice.ID == "next" {
			cursor = page.NextCursor
			continue
		}
		if choice.ID == "advanced" {
			err = actionHomeCommands(command, []string{"tunnel"})
			if interactiveCanceled(err) {
				continue
			}
			return err
		}
		for {
			action, err := chooseHomeAction(command, choice.Title, []selector.Item{
				{ID: "status", Title: "Readiness & health", Description: "Current checks and recovery guidance"},
				{ID: "show", Title: "Details", Description: "Stable endpoint, visibility, and desired state"},
				{ID: "logs", Title: "Recent events", Description: "Redacted tunnel activity"},
				{ID: "route", Title: "Routes", Description: "Origins and matching rules"},
				{ID: "domain", Title: "Domains", Description: "Hostnames, DNS, and certificates"},
				{ID: "connector", Title: "Connectors", Description: "Daemon connections serving this tunnel"},
				{ID: "pause", Title: "Pause", Description: "Stop new traffic; preserve tunnel configuration"},
				{ID: "resume", Title: "Resume", Description: "Resume serving the configured routes"},
				{ID: "delete", Title: "Delete tunnel", Description: "Remove the tunnel; external DNS records are preserved"},
			})
			if interactiveCanceled(err) {
				break
			}
			if err != nil {
				return err
			}
			args := []string{"tunnel", action.ID, choice.ID}
			switch action.ID {
			case "route", "domain", "connector":
				args = []string{"tunnel", action.ID, "list", choice.ID}
			case "delete":
				yes, err := prompt.Confirm(prompt.ConfirmOptions{Title: "Delete " + choice.Title + "?", Description: "This removes the durable tunnel. External DNS records remain.", Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr()})
				if err != nil {
					return err
				}
				if !yes {
					continue
				}
				args = append(args, "--yes", "--wait")
			case "pause", "resume":
				args = append(args, "--wait")
			}
			if err = runHomeResult(command, args); err != nil {
				return err
			}
			if action.ID == "delete" {
				break
			}
		}
	}
}

// The catalog is derived from Cobra, so newly shipped commands are discoverable
// without maintaining another command registry in the home screen.
func interactiveCommandItems(root *cobra.Command, prefix []string) []selector.Item {
	var items []selector.Item
	var walk func(*cobra.Command, []string)
	walk = func(command *cobra.Command, path []string) {
		if command.Hidden || command.Deprecated != "" {
			return
		}
		if len(path) > 0 && command.Runnable() && (len(prefix) == 0 || strings.HasPrefix(strings.Join(path, " ")+" ", strings.Join(prefix, " ")+" ")) {
			items = append(items, selector.Item{ID: strings.Join(path, " "), Title: strings.Join(path, " "), Description: command.Short, Search: command.Use})
		}
		for _, child := range command.Commands() {
			walk(child, append(append([]string(nil), path...), child.Name()))
		}
	}
	walk(root, nil)
	return items
}

func actionHomeCommands(parent *cobra.Command, prefix []string) error {
	for {
		root := newRootCommand()
		choice, err := chooseHomeAction(parent, "Commands", interactiveCommandItems(root, prefix))
		if err != nil {
			return err
		}
		path := strings.Fields(choice.ID)
		selected, _, err := root.Find(path)
		if err != nil {
			return err
		}
		var help bytes.Buffer
		selected.SetOut(&help)
		selected.SetErr(&help)
		_ = selected.Help()
		for {
			action, err := chooseHomeAction(parent, "pb "+choice.Title, []selector.Item{{ID: "run", Title: "Run command", Description: selected.Short}, {ID: "help", Title: "Arguments & options", Description: "Read usage and available flags"}})
			if interactiveCanceled(err) {
				break
			}
			if err != nil {
				return err
			}
			if action.ID == "help" {
				if err := showHomeText(parent, "pb "+choice.Title, help.String()); err != nil {
					return err
				}
				continue
			}
			line, err := prompt.Text(prompt.TextOptions{Title: "pb " + choice.Title, Description: selected.Use + "\nEnter arguments and flags; quotes group text. No shell expansion or execution.", Placeholder: "arguments and flags (empty if none)", Stdin: os.Stdin, Context: parent.Context(), Output: parent.ErrOrStderr(), Validate: func(value string) error { _, err := validatedInteractiveInvocation(path, value); return err }})
			if interactiveCanceled(err) {
				continue
			}
			if err != nil {
				return err
			}
			args, err := validatedInteractiveInvocation(path, line)
			if err != nil {
				return err
			}
			if interactiveStreaming(args) {
				err = executeInteractiveCommand(parent, args)
			} else {
				err = runHomeResult(parent, args)
			}
			if errors.Is(err, selector.ErrInterrupted) {
				return err
			}
			if err != nil && !interactiveCanceled(err) {
				if err = showHomeFailure(parent, err); err != nil {
					return err
				}
			}
		}
	}
}

func validatedInteractiveInvocation(path []string, line string) ([]string, error) {
	values, err := splitPreferenceArgs(line)
	if err != nil {
		return nil, fmt.Errorf("check argument quoting: %w", err)
	}
	args := append(append([]string(nil), path...), values...)
	root := newRootCommand()
	command, remaining, err := root.Find(args)
	if err != nil {
		return nil, err
	}
	if err = command.ParseFlags(remaining); err != nil {
		return nil, err
	}
	if err = command.ValidateArgs(command.Flags().Args()); err != nil {
		return nil, err
	}
	if err = command.ValidateRequiredFlags(); err != nil {
		return nil, err
	}
	return args, nil
}

func interactiveStreaming(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if len(args) == 1 && (args[0] == "config" || args[0] == "env") {
		return true
	}
	switch args[0] {
	case "connect", "ssh", "scp", "sftp", "exec", "preview", "access", "auth", "login", "setup", "pair", "uninstall":
		return true
	}
	for _, arg := range args {
		if arg == "--watch" || arg == "--follow" || arg == "--ephemeral" || arg == "attach" || arg == "login" {
			return true
		}
	}
	return false
}

// Bound retained command output while preserving writer success: truncating a
// view must never cause a completed mutation to be reported as failed.
type homeOutput struct {
	mu        sync.Mutex
	data      []byte
	truncated bool
}

const homeOutputLimit = 128 << 10

func (b *homeOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := min(len(p), homeOutputLimit-len(b.data))
	b.data = append(b.data, p[:n]...)
	b.truncated = b.truncated || n < len(p)
	return len(p), nil
}
func (b *homeOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	value := string(b.data)
	if b.truncated {
		value += "\nOutput truncated. Run this command directly to save its full output."
	}
	return value
}

func runHomeResult(parent *cobra.Command, args []string) error {
	output := &homeOutput{}
	child := newInteractiveRootCommand()
	child.SetIn(parent.InOrStdin())
	child.SetOut(output)
	child.SetErr(parent.ErrOrStderr())
	resolved, ctx, err := preparePreferences(child, interactiveArgs(parent, args), parent.Context())
	if err != nil {
		return err
	}
	child.SetArgs(resolved)
	restore := selector.SuspendScreen(parent.ErrOrStderr())
	err = child.ExecuteContext(ctx)
	restore()
	if interactiveCanceled(err) || errors.Is(err, selector.ErrInterrupted) {
		return err
	}
	text := strings.TrimSpace(output.String())
	if err != nil {
		text += "\n\n" + userFacingError(err)
	} else if text == "" {
		text = "Completed successfully."
	}
	return showHomeText(parent, "pb "+strings.Join(args[:min(2, len(args))], " "), text)
}

func showHomeFailure(command *cobra.Command, err error) error {
	return showHomeText(command, "Action needs attention", userFacingError(err)+"\n\nYour menu is still available. Go back to adjust the input or retry.")
}

type homeTextModel struct {
	ctx            context.Context
	title, content string
	view           viewport.Model
	width, height  int
	interrupted    bool
}

func (m homeTextModel) Init() tea.Cmd { return nil }
func (m homeTextModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width = max(1, msg.Width)
		m.height = max(1, msg.Height)
		m.view.Width = m.width
		m.view.Height = max(1, m.height-4)
		m.view.SetContent(ansi.Hardwrap(m.content, m.width, true))
	case tea.KeyMsg:
		if selector.KeyMatches(m.ctx, "back", msg.String()) || selector.KeyMatches(m.ctx, "select", msg.String()) {
			return m, tea.Quit
		}
		if selector.KeyMatches(m.ctx, "up", msg.String()) {
			m.view.LineUp(1)
			return m, nil
		}
		if selector.KeyMatches(m.ctx, "down", msg.String()) {
			m.view.LineDown(1)
			return m, nil
		}
		switch msg.String() {
		case "ctrl+c":
			m.interrupted = true
			return m, tea.Quit
		case "esc", "q", "enter":
			return m, tea.Quit
		}
	}
	var cmd tea.Cmd
	m.view, cmd = m.view.Update(message)
	return m, cmd
}
func (m homeTextModel) View() string {
	title := selector.TitleStyle(m.ctx).Render(ansi.Truncate(m.title, m.width, "…"))
	keys := selector.HelpKeys(m.ctx)
	rendered := title + "\n\n" + m.view.View() + "\n" + ansi.Truncate(keys["up"]+"/"+keys["down"]+" scroll · PgUp/PgDn page · "+keys["back"]+" back", m.width, "…")
	lines := strings.Split(rendered, "\n")
	return strings.Join(lines[:min(len(lines), max(1, m.height))], "\n")
}
func showHomeText(command *cobra.Command, title, content string) error {
	// Strip terminal escape sequences from command/API output before rendering it
	// as UI content; the viewer owns all terminal control sequences.
	content = ansi.Strip(content)
	content = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r >= 32 && r != 127 {
			return r
		}
		return -1
	}, content)
	view := viewport.New(80, 20)
	view.SetContent(ansi.Hardwrap(content, 80, true))
	options := append(selector.ProgramOptions(os.Stdin, command.ErrOrStderr()), tea.WithContext(command.Context()))
	result, err := tea.NewProgram(homeTextModel{ctx: command.Context(), title: title, content: content, view: view, width: 80, height: 24}, options...).Run()
	if err != nil {
		if errors.Is(err, tea.ErrProgramKilled) && command.Context().Err() != nil {
			return context.Canceled
		}
		return err
	}
	if result.(homeTextModel).interrupted {
		return selector.ErrInterrupted
	}
	return nil
}

var _ io.Writer = (*homeOutput)(nil)
