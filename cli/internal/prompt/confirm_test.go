package prompt

import (
	"context"
	"github.com/pinksaucepasta/paperboat/internal/preferences"
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

func TestConfirmationPreservesEntireConsentInCompactAndNormalViews(t *testing.T) {
	impact := "Assign pull repository " + strings.Repeat("long-repository-name/", 8) + "? Selected content is ordinary plaintext in private Git, and Git history may retain removed versions."
	for _, density := range []string{"comfortable", "compact"} {
		t.Run(density, func(t *testing.T) {
			ctx := preferences.WithContext(context.Background(), preferences.Document{Version: 1, TUI: preferences.TUI{Density: density}})
			updated, _ := (confirmModel{options: ConfirmOptions{Context: ctx, Title: "Confirm configuration change", Description: impact}}).Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			model := updated.(confirmModel)
			view := strings.Join(strings.Fields(ansi.Strip(model.View())), " ")
			if !strings.Contains(view, "ordinary plaintext in private Git") || !strings.Contains(view, "Git history may retain removed versions") {
				t.Fatalf("required consent hidden in %s view", density)
			}
			if model.done || model.yes {
				t.Fatal("rendering consent confirmed mutation")
			}
		})
	}
}

func TestConfirmationLongScopeIsScrollableAndFitsTerminal(t *testing.T) {
	updated, _ := (confirmModel{options: ConfirmOptions{Title: "Confirm", Description: strings.Repeat("repository scope ", 40) + "retained Git history"}}).Update(tea.WindowSizeMsg{Width: 20, Height: 12})
	model := updated.(confirmModel)
	for _, line := range strings.Split(model.View(), "\n") {
		if ansi.StringWidth(line) > 20 {
			t.Fatal("confirmation exceeds terminal width")
		}
	}
	if len(strings.Split(model.View(), "\n")) > 12 {
		t.Fatal("confirmation exceeds terminal height")
	}
	for page := 0; page < 20 && !model.description.AtBottom(); page++ {
		updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyPgDown})
		model = updated.(confirmModel)
	}
	if !strings.Contains(ansi.Strip(model.View()), "Git history") {
		t.Fatal("required scope tail unavailable by scrolling")
	}
	if model.done || model.yes {
		t.Fatal("scrolling confirmed mutation")
	}
	updated, command := model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if command == nil || updated.(confirmModel).yes {
		t.Fatal("scrolled confirmation cannot cancel")
	}
}

func TestConfirmationCannotApproveWhenTerminalCannotShowConsent(t *testing.T) {
	updated, _ := (confirmModel{options: ConfirmOptions{Title: "Confirm", Description: "Required consent"}}).Update(tea.WindowSizeMsg{Width: 20, Height: 3})
	model := updated.(confirmModel)
	if len(strings.Split(model.View(), "\n")) > 3 {
		t.Fatal("small-terminal notice overflows")
	}
	updated, command := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	model = updated.(confirmModel)
	if command != nil || model.done || model.yes {
		t.Fatal("unreadable confirmation was accepted")
	}
	updated, _ = model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	updated, command = updated.(confirmModel).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if command == nil || !updated.(confirmModel).yes {
		t.Fatal("resized confirmation cannot proceed")
	}
}
