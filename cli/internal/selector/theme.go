package selector

import (
	"context"
	"image/color"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/pinksaucepasta/paperboat/cli/internal/preferences"
)

// styleSet is built for each model from its context. Keeping the styles on the
// model path means one command cannot change another command's appearance.
type styleSet struct {
	title          lipgloss.Style
	accent         lipgloss.Style
	subtitle       lipgloss.Style
	selected       lipgloss.Style
	action         lipgloss.Style
	favorite       lipgloss.Style
	favoriteMarker lipgloss.Style
	help           lipgloss.Style
	filter         lipgloss.Style
	error          lipgloss.Style
}

// styleKey is everything a styleSet depends on, so a frame costs one map lookup.
type styleKey struct {
	theme, accent         string
	known, light, noColor bool
}

var (
	styleCache  sync.Map // styleKey -> styleSet
	styleBuilds atomic.Int64
)

// noColor follows https://no-color.org: any non-empty NO_COLOR means mono.
func noColor() bool { return os.Getenv("NO_COLOR") != "" }

func stylesForContext(ctx context.Context) styleSet {
	tui := preferences.FromContext(ctx).TUI
	key := styleKey{theme: tui.Theme, accent: tui.Accent, known: backgroundKnown.Load(), light: lightBackground.Load(), noColor: noColor()}
	if cached, ok := styleCache.Load(key); ok {
		return cached.(styleSet)
	}
	styles := buildStyles(tui, key.noColor)
	styleCache.Store(key, styles)
	return styles
}

func buildStyles(tui preferences.TUI, mono bool) styleSet {
	styleBuilds.Add(1)
	doc := preferences.Document{TUI: tui}
	if mono {
		doc.TUI.Theme = "mono"
	}
	accent := accentColor(doc.TUI)
	accentStyle := lipgloss.NewStyle()
	if accent != nil {
		accentStyle = accentStyle.Foreground(accent)
	}

	styles := styleSet{
		title:          accentStyle.Bold(true),
		accent:         accentStyle,
		subtitle:       lipgloss.NewStyle().Faint(true),
		selected:       lipgloss.NewStyle().Reverse(true),
		action:         accentStyle.Bold(true),
		favorite:       accentStyle.Bold(true),
		favoriteMarker: accentStyle.Bold(true),
		help:           lipgloss.NewStyle().Faint(true),
		filter:         lipgloss.NewStyle().Padding(0, 1),
		error:          lipgloss.NewStyle(),
	}
	if doc.TUI.Theme != "terminal" && doc.TUI.Theme != "mono" {
		background, foreground := "236", "15"
		if doc.TUI.Theme == "light" {
			background, foreground = "254", "0"
		}
		styles.filter = styles.filter.Background(lipgloss.Color(background)).Foreground(lipgloss.Color(foreground))
	}
	if doc.TUI.Theme != "mono" {
		styles.error = styles.error.Foreground(lipgloss.Color("1"))
	}
	return styles
}

// The terminal's background is learned from a running program (RequestBackground
// in Init, ObserveBackground in Update). Until an answer arrives the default
// theme paints no accent colour, which reads on both light and dark terminals.
var (
	lightBackground atomic.Bool
	backgroundKnown atomic.Bool
	backgroundAsked atomic.Bool
)

// RequestBackground returns the OSC 11 query for a program's Init. It asks once
// per screen session: later programs of the session reuse the stored answer, and a
// terminal that never answers is not asked again by every submenu.
func RequestBackground() tea.Cmd {
	if !backgroundQueryable(runtime.GOOS) || backgroundKnown.Load() || !backgroundAsked.CompareAndSwap(false, true) {
		return nil
	}
	return tea.RequestBackgroundColor
}

// backgroundQueryable is false on Windows: conhost and older Windows Terminal builds do
// not answer OSC 11, and the colour only picks an optional accent.
func backgroundQueryable(goos string) bool { return goos != "windows" }

// ObserveBackground records the terminal's background from a program message
// and reports whether it changed, so the model can restyle.
func ObserveBackground(message tea.Msg) bool {
	background, ok := message.(tea.BackgroundColorMsg)
	if !ok {
		return false
	}
	first := !backgroundKnown.Swap(true)
	return lightBackground.Swap(!background.IsDark()) != !background.IsDark() || first
}

func brandColor() color.Color {
	if !backgroundKnown.Load() {
		return nil
	}
	return lipgloss.LightDark(!lightBackground.Load())(lipgloss.Color("#1447E6"), lipgloss.Color("#6F8CFF"))
}

func accentColor(tui preferences.TUI) color.Color {
	if tui.Theme == "mono" {
		return nil
	}
	if tui.Accent != "" {
		return lipgloss.Color(tui.Accent)
	}
	switch tui.Theme {
	case "light":
		return lipgloss.Color("#1447E6")
	case "dark":
		return lipgloss.Color("#6F8CFF")
	case "terminal":
		return nil
	default:
		return brandColor()
	}
}

// TitleStyle returns the context-specific style for a TUI heading.
func TitleStyle(ctx context.Context) lipgloss.Style { return stylesForContext(ctx).title }

// AccentStyle returns the context-specific accent style for a label or action.
func AccentStyle(ctx context.Context) lipgloss.Style {
	return stylesForContext(ctx).accent
}

// HelpStyle returns the context-specific subdued style for help text.
func HelpStyle(ctx context.Context) lipgloss.Style { return stylesForContext(ctx).help }

// ErrorStyle returns the context-specific style for validation and failure text.
func ErrorStyle(ctx context.Context) lipgloss.Style { return stylesForContext(ctx).error }

// Compact reports whether the shared TUI should use one row per item and hide
// optional descriptions.
func Compact(ctx context.Context) bool {
	return preferences.FromContext(ctx).TUI.Density == "compact"
}

func isCompact(ctx context.Context) bool { return Compact(ctx) }

func itemRowHeight(ctx context.Context) int {
	if isCompact(ctx) {
		return 1
	}
	return 2
}

// KeyMatches accepts a configured key in addition to the fixed safety keys.
// Enter/Esc/Ctrl+C remain available even when a preference adds another key.
func KeyMatches(ctx context.Context, action, key string) bool {
	if action == "interrupt" {
		return key == "ctrl+c"
	}
	return slices.Contains(Binding(ctx, action, "").Keys(), key)
}

func actionKeys(ctx context.Context, action string) []string {
	var defaults []string
	switch action {
	case "up":
		defaults = []string{"up", "ctrl+k"}
	case "down":
		defaults = []string{"down", "ctrl+n"}
	case "select":
		defaults = []string{"enter"}
	case "back":
		defaults = []string{"esc"}
	case "interrupt":
		defaults = []string{"ctrl+c"}
	default:
		defaults = nil
	}
	doc := preferences.FromContext(ctx)
	if configured := doc.TUI.Keys[action]; configured != "" && !slices.Contains(defaults, configured) {
		defaults = append(defaults, configured)
	}
	return defaults
}

// HelpKeys returns display-ready effective key labels keyed by the preference
// action names. It includes the fixed controls so footers stay truthful when a
// user adds a custom binding.
func HelpKeys(ctx context.Context) map[string]string {
	keys := make(map[string]string, 8)
	for _, action := range []string{"up", "down", "select", "back", "interrupt"} {
		keys[action] = Binding(ctx, action, "").Help().Key
	}
	for action := range preferences.FromContext(ctx).TUI.Keys {
		if _, ok := keys[action]; !ok {
			keys[action] = keyLabel(preferences.FromContext(ctx).TUI.Keys[action])
		}
	}
	return keys
}

func joinKeyLabels(keys []string) string {
	labels := make([]string, 0, len(keys))
	for _, key := range keys {
		labels = append(labels, keyLabel(key))
	}
	return strings.Join(labels, "/")
}

func keyLabel(key string) string {
	switch key {
	case "up":
		return "↑"
	case "down":
		return "↓"
	case "left":
		return "←"
	case "right":
		return "→"
	case "pgup":
		return "PgUp"
	case "pgdown":
		return "PgDn"
	case "ctrl+c":
		return "Ctrl+C"
	case "esc":
		return "Esc"
	case "enter":
		return "Enter"
	}
	if strings.HasPrefix(key, "ctrl+") || strings.HasPrefix(key, "alt+") {
		parts := strings.SplitN(key, "+", 2)
		modifier := strings.ToUpper(parts[0][:1]) + parts[0][1:]
		value := parts[1]
		if named := keyLabel(value); named != value {
			value = named
		} else if len([]rune(value)) == 1 {
			value = strings.ToUpper(value)
		} else {
			value = strings.ToUpper(value[:1]) + value[1:]
		}
		return modifier + "+" + value
	}
	return key
}

func loadingFooter(ctx context.Context, width int) string {
	return Footer(width, Leave(ctx, "cancel"))
}
