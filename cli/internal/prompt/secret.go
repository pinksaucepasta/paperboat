package prompt

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/pinksaucepasta/paperboat/internal/selector"
)

// SecretOptions describes a hidden, raw single-line input. Unlike Text, Secret
// never trims the value and accepts an empty value after Enter.
type SecretOptions struct {
	Context     context.Context
	Title       string
	Description string
	Placeholder string
	Initial     string
	Stdin       *os.File
	Output      io.Writer
	MaxBytes    int
}

type secretModel struct {
	options   SecretOptions
	input     textinput.Model
	value     []byte
	err       error
	confirmed bool
	canceled  bool
	width     int
}

// Secret reads one hidden value from an interactive terminal. The value is
// only returned to the caller and is never included in the prompt view or
// validation error.
func Secret(options SecretOptions) ([]byte, error) {
	if options.Stdin == nil {
		options.Stdin = os.Stdin
	}
	if err := selector.RequireTerminal(options.Stdin); err != nil {
		return nil, err
	}
	if options.Output == nil {
		options.Output = os.Stderr
	}
	if options.MaxBytes <= 0 {
		options.MaxBytes = 64 << 10
	}
	options.Title = selector.SanitizeText(options.Title)
	options.Description = selector.SanitizeText(options.Description)
	options.Placeholder = selector.SanitizeText(options.Placeholder)
	input := textinput.New()
	input.Placeholder = options.Placeholder
	input.EchoMode = textinput.EchoPassword
	input.EchoCharacter = '•'
	configureInputStyles(&input, options.Context)
	// Allow one extra rune so an over-limit input is rejected instead of being
	// silently truncated by bubbles/textinput.
	input.CharLimit = options.MaxBytes + 1
	input.SetValue(options.Initial)
	input.Focus()
	model := secretModel{options: options, input: input, width: 80}
	programOptions := selector.ProgramOptions(options.Stdin, options.Output)
	if options.Context != nil {
		programOptions = append(programOptions, tea.WithContext(options.Context))
	}
	program := tea.NewProgram(model, programOptions...)
	final, err := program.Run()
	if err != nil {
		if options.Context != nil && options.Context.Err() != nil {
			return nil, options.Context.Err()
		}
		return nil, fmt.Errorf("run hidden input: %w", err)
	}
	result := final.(secretModel)
	if result.canceled || !result.confirmed {
		clear(result.value)
		return nil, ErrCanceled
	}
	return result.value, nil
}

func (m secretModel) Init() tea.Cmd { return textinput.Blink }

func (m secretModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
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
			value := []byte(m.input.Value())
			if len(value) > m.options.MaxBytes {
				clear(value)
				m.err = fmt.Errorf("value exceeds %d bytes", m.options.MaxBytes)
				return m, nil
			}
			m.input.SetValue("")
			m.err = nil
			m.value, m.confirmed = value, true
			return m, tea.Quit
		}
	}
	previous := m.input.Value()
	m.input, command = m.input.Update(message)
	if m.input.Value() != previous {
		m.err = nil
	}
	return m, command
}

func (m secretModel) View() string {
	width := promptWidth(m.width)
	styles := promptStyles(m.options.Context)
	input := m.input
	input.Width = promptInputWidth(width)
	input.Placeholder = selector.SanitizeText(input.Placeholder)
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
