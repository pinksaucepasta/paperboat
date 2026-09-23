package prompt

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
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
	options ConfirmOptions
	yes     bool
	done    bool
	width   int
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
	final, err := tea.NewProgram(confirmModel{options: options, width: 80}, programOptions...).Run()
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
		return m, nil
	}
	if key, ok := message.(tea.KeyMsg); ok {
		value := strings.ToLower(key.String())
		if value == "ctrl+c" || selector.KeyMatches(m.options.Context, "back", value) {
			m.done = true
			return m, tea.Quit
		}
		if selector.KeyMatches(m.options.Context, "select", value) || value == "y" {
			m.yes, m.done = true, true
			return m, tea.Quit
		}
		if value == "n" {
			m.done = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m confirmModel) View() string {
	width := promptWidth(m.width)
	styles := promptStyles(m.options.Context)
	title := styles.title.Render(promptLine(m.options.Title, width))
	lines := []string{title}
	if !selector.Compact(m.options.Context) {
		lines = append(lines, styles.help.Render(promptLine(m.options.Description, width)))
	}
	lines = append(lines, "", promptLine("  Confirm", width), "", styles.help.Render(promptLine(promptConfirmFooter(m.options.Context), width)))
	return strings.Join(lines, "\n")
}

func promptConfirmFooter(ctx context.Context) string {
	keys := selector.HelpKeys(ctx)
	return keys["select"] + "/y confirm  n/" + keys["back"] + "/" + keys["interrupt"] + " cancel"
}
