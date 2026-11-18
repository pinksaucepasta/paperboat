package selector

import (
	"context"
	"image/color"
	"io"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/pinksaucepasta/paperboat/cli/internal/preferences"
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
	view := model.View().Content
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

func TestNoColorEnvironmentStripsEveryThemeColor(t *testing.T) {
	ctx := preferences.WithContext(context.Background(), preferences.Document{Version: 1, TUI: preferences.TUI{Theme: "dark", Accent: "#123456"}})
	if _, ok := TitleStyle(ctx).GetForeground().(lipgloss.NoColor); ok {
		t.Fatal("test needs a coloured baseline")
	}
	t.Setenv("NO_COLOR", "1")
	styles := stylesForContext(ctx)
	for name, style := range map[string]lipgloss.Style{"title": styles.title, "accent": styles.accent, "action": styles.action, "filter": styles.filter, "error": styles.error} {
		if _, ok := style.GetForeground().(lipgloss.NoColor); !ok {
			t.Errorf("NO_COLOR left a %s foreground: %#v", name, style.GetForeground())
		}
		if _, ok := style.GetBackground().(lipgloss.NoColor); !ok {
			t.Errorf("NO_COLOR left a %s background: %#v", name, style.GetBackground())
		}
	}
	if !styles.selected.GetReverse() || !styles.title.GetBold() {
		t.Fatal("NO_COLOR must keep the non-colour emphasis")
	}
	t.Setenv("NO_COLOR", "")
	if _, ok := TitleStyle(ctx).GetForeground().(lipgloss.NoColor); ok {
		t.Fatal("an empty NO_COLOR must not strip colour")
	}
}

func TestStylesAreBuiltOncePerThemeNotPerFrame(t *testing.T) {
	ctx := preferences.WithContext(context.Background(), preferences.Document{Version: 1, TUI: preferences.TUI{Theme: "light", Accent: "#0a0b0c"}})
	stylesForContext(ctx)
	before := styleBuilds.Load()
	for range 100 {
		stylesForContext(ctx)
	}
	if built := styleBuilds.Load() - before; built != 0 {
		t.Fatalf("styles rebuilt %d times for an unchanged theme", built)
	}
}

func resetBackground(t *testing.T) {
	t.Helper()
	backgroundKnown.Store(false)
	backgroundAsked.Store(false)
	lightBackground.Store(false)
	t.Cleanup(func() { backgroundKnown.Store(false); backgroundAsked.Store(false); lightBackground.Store(false) })
}

func TestBackgroundIsQueriedOncePerSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows terminals are never asked (TestBackgroundIsNotQueriedOnWindows)")
	}
	resetBackground(t)
	if RequestBackground() == nil {
		t.Fatal("the first program of a session must ask for the background")
	}
	for range 3 {
		if RequestBackground() != nil {
			t.Fatal("a later submenu asked again")
		}
	}
	end := BeginScreen(io.Discard)
	if RequestBackground() == nil {
		t.Fatal("a new screen session must be able to ask again")
	}
	end()
	ObserveBackground(tea.BackgroundColorMsg{Color: color.White})
	end = BeginScreen(io.Discard)
	defer end()
	if RequestBackground() != nil {
		t.Fatal("an answered background was asked for again")
	}
}

func TestFirstPaintHasNoAccentUntilBackgroundIsKnown(t *testing.T) {
	resetBackground(t)
	// An unset theme uses the adaptive brand accent (explicit themes pin their own).
	if got := accentColor(preferences.TUI{}); got != nil {
		t.Fatalf("accent painted before the background is known: %#v", got)
	}
	if !ObserveBackground(tea.BackgroundColorMsg{Color: color.White}) {
		t.Fatal("the first answer must report a change so the model restyles")
	}
	light := accentColor(preferences.TUI{})
	ObserveBackground(tea.BackgroundColorMsg{Color: color.Black})
	if dark := accentColor(preferences.TUI{}); light == nil || dark == nil || reflect.DeepEqual(light, dark) {
		t.Fatalf("light and dark accents = %#v / %#v", light, dark)
	}
}

func TestFooterIsDrawnFromTheBindingsThatHandleKeys(t *testing.T) {
	ctx := preferences.WithContext(context.Background(), preferences.Document{Version: 1, TUI: preferences.TUI{Keys: map[string]string{"select": "ctrl+j", "back": "alt+left"}}})
	footer := Footer(0, Move(ctx, "move"), Binding(ctx, "select", "select"), Binding(ctx, "back", "back"), Leave(ctx, "cancel"))
	for _, want := range []string{"↑/↓ move", "Enter/Ctrl+J select", "Esc/Alt+← back", "Esc/Alt+←/Ctrl+C cancel"} {
		if !strings.Contains(footer, want) {
			t.Errorf("footer %q lacks %q", footer, want)
		}
	}
	custom := preferences.WithContext(context.Background(), preferences.Document{Version: 1, TUI: preferences.TUI{Keys: map[string]string{"up": "ctrl+u"}}})
	if got := Move(custom, "move"); got.Help().Key != "↑/↓/Ctrl+U" || !key.Matches(tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl}, got) {
		t.Errorf("move label %q must name the added key and still match the built-in aliases", got.Help().Key)
	}
	// every key a footer names is matched by the handler, and the reverse.
	for _, key := range []string{"enter", "ctrl+j"} {
		if !KeyMatches(ctx, "select", key) {
			t.Errorf("footer names %q but select ignores it", key)
		}
	}
	if KeyMatches(ctx, "select", "y") || KeyMatches(ctx, "select", "q") {
		t.Error("select matched a key the footer does not name")
	}
	if got := Footer(30, Move(ctx, "move"), Binding(ctx, "select", "select")); strings.Contains(got, "sele") && !strings.Contains(got, "select") {
		t.Errorf("footer cut an item in half: %q", got)
	}
	if got := Footer(30, Move(ctx, "move"), Binding(ctx, "select", "select")); ansi.StringWidth(got) > 30 {
		t.Errorf("footer is %d wide at width 30: %q", ansi.StringWidth(got), got)
	}
}

func TestBackgroundIsNotQueriedOnWindows(t *testing.T) {
	if backgroundQueryable("windows") || !backgroundQueryable("linux") || !backgroundQueryable("darwin") {
		t.Fatal("OSC 11 must be skipped on Windows only")
	}
}
