package prompt

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func TestTextModelValidatesBeforeQuitting(t *testing.T) {
	model := textModel{options: TextOptions{Validate: func(value string) error {
		if value == "" {
			return errors.New("required")
		}
		return nil
	}}, width: 80}
	model.input.SetValue("")
	updated, command := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	result := updated.(textModel)
	if command != nil || result.confirmed || result.err == nil {
		t.Fatalf("invalid input = confirmed %t error %v", result.confirmed, result.err)
	}
	result.input.SetValue("paperboat")
	updated, _ = result.Update(tea.KeyMsg{Type: tea.KeyEnter})
	result = updated.(textModel)
	if !result.confirmed || result.value != "paperboat" {
		t.Fatalf("valid input = confirmed %t value %q", result.confirmed, result.value)
	}
}

func TestTextRejectsNonTerminalInput(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()

	if _, err := Text(TextOptions{Stdin: reader, Output: io.Discard}); !errors.Is(err, ErrNotTerminal) {
		t.Fatalf("text error=%v, want ErrNotTerminal", err)
	}
}

func TestTextModelClearsValidationErrorWhenEdited(t *testing.T) {
	model := textModel{options: TextOptions{Validate: func(string) error { return errors.New("invalid") }}, width: 24}
	model.input = textinput.New()
	model.input.Focus()
	updated, command := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	result := updated.(textModel)
	if command != nil || result.err == nil {
		t.Fatalf("invalid input command=%v error=%v", command, result.err)
	}
	updated, _ = result.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	result = updated.(textModel)
	if result.err != nil {
		t.Fatalf("editing left stale validation error: %v", result.err)
	}
}

func TestTextViewFitsNarrowWidth(t *testing.T) {
	model := textModel{options: TextOptions{Title: strings.Repeat("title", 8), Description: strings.Repeat("description", 8)}, width: 7}
	model.input = textinput.New()
	model.input.Placeholder = strings.Repeat("path", 8)
	for _, line := range strings.Split(model.View(), "\n") {
		if width := ansi.StringWidth(line); width > 7 {
			t.Fatalf("line width=%d: %q", width, line)
		}
	}
}
