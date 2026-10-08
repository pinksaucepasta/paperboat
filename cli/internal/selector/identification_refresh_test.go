package selector

import (
	"context"
	"errors"
	"github.com/charmbracelet/bubbles/textinput"
	"strings"
	"testing"
)

func TestLiveChoicesPreserveSelectionAndFilter(t *testing.T) {
	input := textinput.New()
	input.SetValue("api")
	m := chooserModel{options: Options{Context: context.Background(), Subtitle: "Sessions"}, input: input, choices: NewModel([]Item{{ID: "a", Title: "one", Search: "api"}, {ID: "b", Title: "two", Search: "api"}}, 8)}
	m.choices.SetFilter("api")
	m.choices.Move(1)
	updated, _ := m.Update(choicesRefreshed{items: []Item{{ID: "b", Title: "two", Description: "✳ Working", Search: "api"}, {ID: "a", Title: "one", Search: "api"}}})
	m = updated.(chooserModel)
	selected, ok := m.choices.Selected()
	if !ok || selected.ID != "b" || selected.Description != "✳ Working" || m.input.Value() != "api" {
		t.Fatal("refresh lost selection/filter or metadata")
	}
	updated, _ = m.Update(choicesRefreshed{err: errors.New("unavailable")})
	m = updated.(chooserModel)
	if !strings.Contains(m.options.Subtitle, "showing last observation") {
		t.Fatal("failed refresh presented stale metadata as current")
	}
	selected, _ = m.choices.Selected()
	if selected.ID != "b" {
		t.Fatal("failed refresh destroyed selection")
	}
	updated, _ = m.Update(choicesRefreshed{items: []Item{{ID: "b", Title: "two", Description: "\x1b[2Jbad\nvalue", Search: "api"}}})
	m = updated.(chooserModel)
	selected, _ = m.choices.Selected()
	if strings.ContainsAny(selected.Description, "\x1b\n") || strings.Contains(m.options.Subtitle, "refresh failed") {
		t.Fatal("refreshed text unsafe or error not cleared")
	}
}
