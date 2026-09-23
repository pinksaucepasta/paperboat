package prompt

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/pinksaucepasta/paperboat/internal/preferences"
)

func TestPromptContextKeysKeepSafetyControlsAndCompactLayout(t *testing.T) {
	ctx := preferences.WithContext(context.Background(), preferences.Document{
		Version: 1,
		TUI: preferences.TUI{
			Theme:   "mono",
			Density: "compact",
			Keys:    map[string]string{"select": "ctrl+j", "back": "alt+left"},
		},
	})
	model := textModel{
		options: TextOptions{Context: ctx, Title: "Input", Description: "optional details", Validate: func(string) error { return nil }},
		input:   newPromptInput(),
		width:   40,
	}
	model.input.SetValue("value")
	updated, command := model.Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
	result := updated.(textModel)
	if command == nil || !result.confirmed || result.value != "value" {
		t.Fatalf("custom select confirmed=%t value=%q command=%v", result.confirmed, result.value, command)
	}
	model = textModel{options: TextOptions{Context: ctx, Title: "Input", Description: "optional details"}, input: newPromptInput(), width: 40}
	updated, command = model.Update(tea.KeyMsg{Type: tea.KeyLeft, Alt: true})
	result = updated.(textModel)
	if command == nil || !result.canceled {
		t.Fatalf("custom back canceled=%t command=%v", result.canceled, command)
	}
	model = textModel{options: TextOptions{Context: ctx, Title: "Input", Description: "optional details"}, input: newPromptInput(), width: 40}
	if updated, command = model.Update(tea.KeyMsg{Type: tea.KeyCtrlC}); command == nil || !updated.(textModel).canceled {
		t.Fatal("Ctrl+C safety control was not preserved")
	}
	view := result.View()
	if strings.Contains(view, "optional details") {
		t.Fatalf("compact prompt rendered description: %q", view)
	}
}

func newPromptInput() textinput.Model {
	input := textinput.New()
	input.Focus()
	return input
}
