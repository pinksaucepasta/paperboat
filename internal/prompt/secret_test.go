package prompt

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func TestSecretModelPreservesRawWhitespaceAndAllowsEmpty(t *testing.T) {
	model := secretModel{options: SecretOptions{MaxBytes: 32_767}, width: 80}
	model.input.EchoMode = textinput.EchoPassword
	model.input.EchoCharacter = '•'
	model.input.SetValue("  canary value  ")
	updated, command := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	result := updated.(secretModel)
	if command == nil || !result.confirmed || string(result.value) != "  canary value  " {
		t.Fatalf("confirmed=%t value=%q command=%v", result.confirmed, result.value, command)
	}
	if strings.Contains(result.View(), "canary value") {
		t.Fatalf("hidden input view leaked value: %q", result.View())
	}

	model = secretModel{options: SecretOptions{MaxBytes: 32_767}, width: 80}
	model.input.EchoMode = textinput.EchoPassword
	model.input.EchoCharacter = '•'
	model.input.SetValue("")
	updated, command = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	result = updated.(secretModel)
	if command == nil || !result.confirmed || len(result.value) != 0 {
		t.Fatalf("empty value was not accepted: confirmed=%t value=%q", result.confirmed, result.value)
	}
}

func TestSecretModelRejectsValueOverByteLimit(t *testing.T) {
	model := secretModel{options: SecretOptions{MaxBytes: 3}, width: 80}
	model.input.SetValue("1234")
	updated, command := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	result := updated.(secretModel)
	if command != nil || result.confirmed || result.err == nil || !strings.Contains(result.err.Error(), "3 bytes") {
		t.Fatalf("oversized value accepted: confirmed=%t err=%v command=%v", result.confirmed, result.err, command)
	}
}

func TestSecretModelClearsLimitErrorWhenEdited(t *testing.T) {
	model := secretModel{options: SecretOptions{MaxBytes: 3}, width: 24}
	model.input = textinput.New()
	model.input.Focus()
	model.input.SetValue("1234")
	updated, command := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	result := updated.(secretModel)
	if command != nil || result.err == nil {
		t.Fatalf("oversized value command=%v error=%v", command, result.err)
	}
	updated, _ = result.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	result = updated.(secretModel)
	if result.err != nil {
		t.Fatalf("editing left stale limit error: %v", result.err)
	}
}

func TestSecretViewFitsNarrowWidth(t *testing.T) {
	model := secretModel{options: SecretOptions{Title: strings.Repeat("secret", 8), Description: strings.Repeat("description", 8)}, width: 7}
	model.input = textinput.New()
	model.input.Placeholder = strings.Repeat("value", 8)
	for _, line := range strings.Split(model.View(), "\n") {
		if width := ansi.StringWidth(line); width > 7 {
			t.Fatalf("line width=%d: %q", width, line)
		}
	}
}
