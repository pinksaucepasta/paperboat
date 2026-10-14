package selector

import (
	"context"
	"slices"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/pinksaucepasta/paperboat/internal/preferences"
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

func stylesForContext(ctx context.Context) styleSet {
	doc := preferences.FromContext(ctx)
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

func accentColor(tui preferences.TUI) lipgloss.TerminalColor {
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
		return brandColor
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
	for _, candidate := range actionKeys(ctx, action) {
		if key == candidate {
			return true
		}
	}
	return false
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
		keys[action] = joinKeyLabels(actionKeys(ctx, action))
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

func loadingFooter(ctx context.Context) string {
	keys := HelpKeys(ctx)
	return keys["back"] + "/" + keys["interrupt"] + " cancel"
}
