package prompt

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/pinksaucepasta/paperboat/internal/selector"
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
	if options.Output == nil {
		options.Output = os.Stderr
	}
	options.Title = selector.SanitizeText(options.Title)
	options.Description = selector.SanitizeText(options.Description)
	programOptions := selector.ProgramOptions(options.Stdin, options.Output)
	if options.Context != nil {
		programOptions = append(programOptions, tea.WithContext(options.Context))
	}
	model := confirmModel{options: options, width: 80, height: 24, description: viewport.New(80, 17)}
	model.description.SetContent(ansi.Wrap(options.Description, 80, ""))
	final, err := tea.NewProgram(model, programOptions...).Run()
	if err != nil {
		if options.Context != nil && options.Context.Err() != nil {
			return false, options.Context.Err()
		}
		return false, fmt.Errorf("run confirmation: %w", err)
	}
	result := final.(confirmModel)
	return result.done && result.yes, nil
}

func (m confirmModel) Init() tea.Cmd { return nil }

func (m confirmModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if size, ok := message.(tea.WindowSizeMsg); ok {
		m.width = promptWidth(size.Width)
		m.height = max(1, size.Height)
		m.description.Width = m.width
		m.description.Height = max(1, m.height-7)
		m.description.SetContent(ansi.Wrap(m.options.Description, m.width, ""))
		return m, nil
	}
	if key, ok := message.(tea.KeyMsg); ok {
		value := strings.ToLower(key.String())
		if value == "ctrl+c" || selector.KeyMatches(m.options.Context, "back", value) {
			m.done = true
			return m, tea.Quit
		}
		if selector.KeyMatches(m.options.Context, "select", value) || value == "y" {
			if m.height > 0 && m.height < 8 {
				return m, nil
			}
			m.yes, m.done = true, true
			return m, tea.Quit
		}
		if value == "n" {
			m.done = true
			return m, tea.Quit
		}
		if selector.KeyMatches(m.options.Context, "up", value) {
			m.description.LineUp(1)
			return m, nil
		}
		if selector.KeyMatches(m.options.Context, "down", value) {
			m.description.LineDown(1)
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.description, cmd = m.description.Update(message)
	return m, cmd
}

func (m confirmModel) View() string {
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
	lines = append(lines, "", promptLine("  Confirm", width), "", styles.help.Render(promptLine(promptConfirmFooter(m.options.Context), width)))
	if m.description.TotalLineCount() > m.description.Height {
		lines = append(lines, styles.help.Render(promptLine(selector.HelpKeys(m.options.Context)["up"]+"/"+selector.HelpKeys(m.options.Context)["down"]+" · PgUp/PgDn scroll", width)))
	}
	return strings.Join(lines, "\n")
}

func promptConfirmFooter(ctx context.Context) string {
	keys := selector.HelpKeys(ctx)
	return keys["select"] + "/y confirm  n/" + keys["back"] + "/" + keys["interrupt"] + " cancel"
}
