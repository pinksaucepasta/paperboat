package prompt

import (
	"context"
	"io"
	"os"
	"slices"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/pinksaucepasta/paperboat/cli/internal/selector"
)

type ConfirmOptions struct {
	Context     context.Context
	Title       string
	Description string
	Stdin       *os.File
	Output      io.Writer
}

type confirmModel struct {
	options     ConfirmOptions
	yes         bool
	done        bool
	width       int
	height      int
	description viewport.Model
}

func Confirm(options ConfirmOptions) (bool, error) {
	if options.Stdin == nil {
		options.Stdin = os.Stdin
	}
	if err := selector.RequireTerminal(options.Stdin); err != nil {
		return false, err
	}
	if err := canceledBeforeRun(options.Context, "run confirmation failed"); err != nil {
		return false, err
	}
	if options.Output == nil {
		options.Output = os.Stderr
	}
	options.Title = selector.SanitizeText(options.Title)
	options.Description = selector.SanitizeText(options.Description)
	programOptions := selector.ProgramOptions(options.Stdin, options.Output)
	if options.Context != nil {
		programOptions = append(programOptions, tea.WithContext(options.Context))
	}
	sized, _ := confirmModel{options: options, description: viewport.New()}.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	final, err := tea.NewProgram(sized, programOptions...).Run()
	if err != nil {
		return false, runFailure(options.Context, err, "run confirmation failed")
	}
	result := final.(confirmModel)
	return result.done && result.yes, nil
}

func (m confirmModel) Init() tea.Cmd { return selector.RequestBackground() }

func (m confirmModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	selector.ObserveBackground(message)
	if size, ok := message.(tea.WindowSizeMsg); ok {
		m.width = promptWidth(size.Width)
		m.height = max(1, size.Height)
		m.description.SetWidth(m.width)
		wrapped := ansi.Wrap(m.options.Description, m.width, "")
		// Size the viewport to the text so the control sits right under it.
		m.description.SetHeight(max(1, min(strings.Count(wrapped, "\n")+1, m.height-7)))
		m.description.SetContent(wrapped)
		return m, nil
	}
	if press, ok := message.(tea.KeyPressMsg); ok {
		value := strings.ToLower(press.String())
		yes, no := m.bindings()
		if value == "ctrl+c" || selector.KeyMatches(m.options.Context, "back", value) {
			m.done = true
			return m, tea.Quit
		}
		if slices.Contains(yes.Keys(), value) {
			if m.height > 0 && m.height < 8 {
				return m, nil
			}
			m.yes, m.done = true, true
			return m, tea.Quit
		}
		if slices.Contains(no.Keys(), value) {
			m.done = true
			return m, tea.Quit
		}
		if selector.KeyMatches(m.options.Context, "up", value) {
			m.description.ScrollUp(1)
			return m, nil
		}
		if selector.KeyMatches(m.options.Context, "down", value) {
			m.description.ScrollDown(1)
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.description, cmd = m.description.Update(message)
	return m, cmd
}

func (m confirmModel) View() tea.View { return selector.NewView(m.render()) }

func (m confirmModel) render() string {
	width := promptWidth(m.width)
	styles := promptStyles(m.options.Context)
	if m.height > 0 && m.height < 8 {
		return promptLine("Enlarge terminal to read confirmation; Esc cancels", width)
	}
	title := styles.title.Render(promptLine(m.options.Title, width))
	lines := []string{title}
	if m.options.Description != "" {
		// Consent and mutation scope are required information in every theme,
		// including compact mode. Keep all text available through scrolling.
		lines = append(lines, "", styles.help.Render(m.description.View()))
	}
	lines = append(lines, "", promptLine("  Confirm", width), "", styles.help.Render(promptLine(promptConfirmFooter(m, width), width)))
	if m.description.TotalLineCount() > m.description.Height() {
		lines = append(lines, styles.help.Render(promptLine(selector.Footer(width, selector.Move(m.options.Context, "scroll"), selector.Hint("PgUp/PgDn", "page", "pgup", "pgdown")), width)))
	}
	return strings.Join(lines, "\n")
}

// bindings are the confirm and cancel keys: matching and the footer both use them.
func (m confirmModel) bindings() (yes, no key.Binding) {
	ctx := m.options.Context
	return selector.Extend(selector.Binding(ctx, "select", "confirm"), "confirm", "y"), selector.Extend(selector.Leave(ctx, "cancel"), "cancel", "n")
}

func promptConfirmFooter(m confirmModel, width int) string {
	yes, no := m.bindings()
	return selector.Footer(width, yes, no)
}
