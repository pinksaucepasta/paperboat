package selector

import (
	"context"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/pinksaucepasta/paperboat/internal/preferences"
)

func TestThemeAndKeysStayIsolatedPerContext(t *testing.T) {
	dark := preferences.WithContext(context.Background(), preferences.Document{
		Version: 1,
		TUI: preferences.TUI{
			Theme:  "dark",
			Accent: "#123456",
			Keys:   map[string]string{"up": "ctrl+u", "select": "ctrl+j", "back": "alt+left"},
		},
	})
	mono := preferences.WithContext(context.Background(), preferences.Document{
		Version: 1,
		TUI:     preferences.TUI{Theme: "mono", Keys: map[string]string{"down": "ctrl+d"}},
	})

	darkKeys := HelpKeys(dark)
	if darkKeys["up"] != "↑/Ctrl+K/Ctrl+U" || darkKeys["select"] != "Enter/Ctrl+J" || darkKeys["back"] != "Esc/Alt+←" {
		t.Fatalf("dark effective keys=%v", darkKeys)
	}
	if !KeyMatches(dark, "up", "ctrl+u") || !KeyMatches(dark, "select", "ctrl+j") || !KeyMatches(dark, "back", "esc") {
		t.Fatal("configured or fixed key was not recognized")
	}
	if KeyMatches(dark, "interrupt", "esc") || !KeyMatches(dark, "interrupt", "ctrl+c") {
		t.Fatal("interrupt safety key handling changed")
	}
	if got := HelpKeys(mono); got["down"] != "↓/Ctrl+N/Ctrl+D" {
		t.Fatalf("mono effective keys=%v", got)
	}
	monoKeys := HelpKeys(mono)
	monoKeys["down"] = "mutated"
	if HelpKeys(mono)["down"] == "mutated" {
		t.Fatal("help key result aliases context state")
	}
	if reflect.DeepEqual(TitleStyle(dark).GetForeground(), TitleStyle(mono).GetForeground()) {
		t.Fatalf("contexts share title foreground: dark=%#v mono=%#v", TitleStyle(dark).GetForeground(), TitleStyle(mono).GetForeground())
	}
	if _, ok := TitleStyle(mono).GetForeground().(lipgloss.NoColor); !ok {
		t.Fatalf("mono title foreground=%#v, want no color", TitleStyle(mono).GetForeground())
	}
}

func TestChooserCompactDensityUsesOneRowAndHidesDescription(t *testing.T) {
	ctx := preferences.WithContext(context.Background(), preferences.Document{Version: 1, TUI: preferences.TUI{Density: "compact"}})
	model := chooserModel{
		options: Options{Context: ctx, Title: "Machines"},
		choices: NewModel([]Item{{ID: "one", Title: "One", Description: "hidden detail"}, {ID: "two", Title: "Two"}}, 2),
		width:   40,
		height:  10,
	}
	if index, ok := model.itemAtRow(3); !ok || index != 1 {
		t.Fatalf("compact row hit index=%d ok=%t", index, ok)
	}
	if _, ok := model.itemAtRow(4); ok {
		t.Fatal("compact row below list was clickable")
	}
	view := model.View()
	if containsPlain(view, "hidden detail") {
		t.Fatalf("compact view rendered optional description: %q", view)
	}
}

func TestChooserResizeUsesDensityForMouseRows(t *testing.T) {
	ctx := preferences.WithContext(context.Background(), preferences.Document{Version: 1, TUI: preferences.TUI{Density: "compact"}})
	model := chooserModel{
		options: Options{Context: ctx, Title: "Machines", Subtitle: "Choose"},
		choices: NewModel([]Item{{ID: "one"}, {ID: "two"}, {ID: "three"}}, 8),
		width:   80,
		height:  24,
	}
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 6})
	resized := updated.(chooserModel)
	if resized.choices.rows != 1 {
		t.Fatalf("compact rows after resize=%d, want 1", resized.choices.rows)
	}
	if index, ok := resized.itemAtRow(3); !ok || index != 0 {
		t.Fatalf("resized compact hit index=%d ok=%t", index, ok)
	}
}

func containsPlain(value, want string) bool {
	return strings.Contains(ansi.Strip(value), want)
}
