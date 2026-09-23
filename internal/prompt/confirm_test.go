package prompt

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func TestConfirmModelAcceptsConfirmationKeys(t *testing.T) {
	for _, key := range []tea.KeyMsg{{Type: tea.KeyEnter}, {Type: tea.KeyRunes, Runes: []rune{'y'}}} {
		updated, command := (confirmModel{}).Update(key)
		result := updated.(confirmModel)
		if command == nil || !result.done || !result.yes {
			t.Fatalf("key %q = command %v, done %t, yes %t", key.String(), command, result.done, result.yes)
		}
	}
}

func TestConfirmModelAcceptsCancellationKeys(t *testing.T) {
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'n'}},
		{Type: tea.KeyEsc},
		{Type: tea.KeyCtrlC},
	} {
		updated, command := (confirmModel{}).Update(key)
		result := updated.(confirmModel)
		if command == nil || !result.done || result.yes {
			t.Fatalf("key %q = command %v, done %t, yes %t", key.String(), command, result.done, result.yes)
		}
	}
}

func TestConfirmViewFitsNarrowWidth(t *testing.T) {
	model := confirmModel{options: ConfirmOptions{Title: strings.Repeat("confirm", 8), Description: strings.Repeat("description", 8)}, width: 7}
	for _, line := range strings.Split(model.View(), "\n") {
		if width := ansi.StringWidth(line); width > 7 {
			t.Fatalf("line width=%d: %q", width, line)
		}
	}
}
