// Package prompt provides focused Bubble Tea inputs for interactive workflows.
package prompt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/pinksaucepasta/paperboat/internal/selector"
)

var ErrCanceled = errors.New("input canceled")
var ErrNotTerminal = selector.ErrNotTerminal

type TextOptions struct {
	Context     context.Context
	Title       string
	Description string
	Placeholder string
	Initial     string
	Stdin       *os.File
	Output      io.Writer
	Validate    func(string) error
}

type textModel struct {
	options   TextOptions
	input     textinput.Model
	value     string
	err       error
	confirmed bool
	canceled  bool
	width     int
}

func Text(options TextOptions) (string, error) {
	if options.Stdin == nil {
		options.Stdin = os.Stdin
	}
	if err := selector.RequireTerminal(options.Stdin); err != nil {
		return "", err
	}
	if options.Output == nil {
		options.Output = os.Stderr
	}
	options.Title = selector.SanitizeText(options.Title)
	options.Description = selector.SanitizeText(options.Description)
	options.Placeholder = selector.SanitizeText(options.Placeholder)
	options.Initial = selector.SanitizeText(options.Initial)
	input := textinput.New()
	input.Placeholder = options.Placeholder
	configureInputStyles(&input, options.Context)
	input.SetValue(options.Initial)
	input.Focus()
	model := textModel{options: options, input: input, width: 80}
	programOptions := selector.ProgramOptions(options.Stdin, options.Output)
	if options.Context != nil {
		programOptions = append(programOptions, tea.WithContext(options.Context))
	}
	program := tea.NewProgram(model, programOptions...)
	final, err := program.Run()
	if err != nil {
		if options.Context != nil && options.Context.Err() != nil {
			return "", options.Context.Err()
		}
		return "", fmt.Errorf("run input: %w", err)
	}
	result := final.(textModel)
	if result.canceled || !result.confirmed {
		return "", ErrCanceled
	}
	return result.value, nil
}

func (m textModel) Init() tea.Cmd { return textinput.Blink }

func (m textModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	var command tea.Cmd
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width = promptWidth(message.Width)
		m.input.Width = promptInputWidth(m.width)
	case tea.KeyMsg:
		key := message.String()
		if key == "ctrl+c" || selector.KeyMatches(m.options.Context, "back", key) {
			m.canceled = true
			return m, tea.Quit
		}
		if selector.KeyMatches(m.options.Context, "select", key) {
			value := strings.TrimSpace(selector.SanitizeText(m.input.Value()))
			if m.options.Validate != nil {
				if err := m.options.Validate(value); err != nil {
					m.err = err
					return m, nil
				}
			}
			m.err = nil
			m.value, m.confirmed = value, true
			return m, tea.Quit
		}
	}
	previous := m.input.Value()
	m.input, command = m.input.Update(message)
	if value := selector.SanitizeText(m.input.Value()); value != m.input.Value() {
		m.input.SetValue(value)
	}
	if m.input.Value() != previous {
		m.err = nil
	}
	return m, command
}

func (m textModel) View() string {
	width := promptWidth(m.width)
	styles := promptStyles(m.options.Context)
	input := m.input
	input.Width = promptInputWidth(width)
	input.Placeholder = selector.SanitizeText(input.Placeholder)
	input.SetValue(selector.SanitizeText(input.Value()))
	title := styles.title.Render(promptLine(m.options.Title, width))
	lines := []string{title}
	if !selector.Compact(m.options.Context) {
		lines = append(lines, styles.help.Render(promptLine(m.options.Description, width)))
	}
	lines = append(lines, "", promptInputView(input.View(), width))
	if m.err != nil {
		lines = append(lines, "", styles.err.Render(promptLine("  "+m.err.Error(), width)))
	}
	lines = append(lines, "", styles.help.Render(promptLine(promptFooter(m.options.Context, "continue", "cancel"), width)))
	return strings.Join(lines, "\n")
}
