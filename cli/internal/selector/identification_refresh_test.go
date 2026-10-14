package selector

import (
	"context"
	"errors"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"strings"
	"syscall"
	"testing"
)

func TestLiveChoicesPreserveSelectionAndFilter(t *testing.T) {
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) { faults = append(faults, f) })
	defer restore()
	reference := supportref.New()
	ctx := supportref.WithContext(context.Background(), reference)
	input := textinput.New()
	input.SetValue("api")
	m := chooserModel{options: Options{Context: ctx, Subtitle: "Sessions"}, input: input, choices: NewModel([]Item{{ID: "a", Title: "one", Search: "api"}, {ID: "b", Title: "two", Search: "api"}}, 8)}
	m.choices.SetFilter("api")
	m.choices.Move(1)
	updated, _ := m.Update(choicesRefreshed{items: []Item{{ID: "b", Title: "two", Description: "✳ Working", Search: "api"}, {ID: "a", Title: "one", Search: "api"}}})
	m = updated.(chooserModel)
	selected, ok := m.choices.Selected()
	if !ok || selected.ID != "b" || selected.Description != "✳ Working" || m.input.Value() != "api" {
		t.Fatal("refresh lost selection/filter or metadata")
	}
	updated, _ = m.Update(choicesRefreshed{err: errors.Join(errors.New("PRIVATE unavailable"), syscall.EIO)})
	m = updated.(chooserModel)
	if !strings.Contains(m.options.Subtitle, "showing last observation") {
		t.Fatal("failed refresh presented stale metadata as current")
	}
	if len(faults) != 1 || faults[0].Errno != int(syscall.EIO) || faults[0].SupportReference != reference {
		t.Fatalf("refresh failure not correlated: %+v", faults)
	}
	updated, _ = m.Update(choicesRefreshed{err: syscall.EIO})
	m = updated.(chooserModel)
	if len(faults) != 1 {
		t.Fatal("refresh episode duplicated observation")
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
	panicCause := errors.New("PRIVATE refresh panic")
	m.options.Refresh = func(context.Context) ([]Item, error) { panic(panicCause) }
	_, refresh := m.Update(choicesRefreshTick{})
	msg := refresh()
	refreshed, ok := msg.(choicesRefreshed)
	if !ok || !errors.Is(refreshed.err, tea.ErrProgramPanic) || !errors.Is(refreshed.err, panicCause) || strings.Contains(refreshed.err.Error(), "PRIVATE refresh panic") {
		t.Fatalf("refresh panic was not contained with safe cause retention: %#v", msg)
	}
}

func TestRefreshRecoveryAllowsNextFailureEpisode(t *testing.T) {
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) { faults = append(faults, f) })
	defer restore()
	m := chooserModel{options: Options{Context: context.Background()}, choices: NewModel(nil, 8), input: textinput.New()}
	for _, msg := range []choicesRefreshed{{err: syscall.EIO}, {items: []Item{{ID: "one"}}}, {err: syscall.EIO}} {
		updated, _ := m.Update(msg)
		m = updated.(chooserModel)
	}
	if len(faults) != 2 {
		t.Fatalf("recovery did not reset episode: %d", len(faults))
	}
}
