package statusbar

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

func (b *Bar) textLocked() string {
	project, session, connection := b.identityLocked()
	if b.privacy {
		project, session = "private", "session"
	}
	identity := " " + project + " / " + session + " / " + connection
	if failure := b.currentFailureLocked(); failure != "" {
		return identity + " / " + failure + " "
	}
	if b.noticeActiveLocked() {
		return " Paperboat: " + b.notice + " "
	}
	return identity + " "
}

func (b *Bar) identityLocked() (project, session, connection string) {
	project, session, connection = b.project, b.session, b.connection
	if project == "" {
		project = "project"
	}
	if session == "" {
		session = "default"
	}
	if connection == "" {
		connection = "connecting"
	}
	return project, session, connection
}

func (b *Bar) layoutLocked(width int) string {
	layout := Layout{Left: append([]string(nil), b.layout.Left...), Center: append([]string(nil), b.layout.Center...), Right: append([]string(nil), b.layout.Right...)}
	for _, disposable := range []string{"storage", "credits", "config_sync", "session", "project"} {
		left, center, right := b.layoutRegionsLocked(layout)
		if ansi.StringWidth(left)+ansi.StringWidth(center)+ansi.StringWidth(right)+4 <= width {
			break
		}
		layout.Left = withoutWidget(layout.Left, disposable)
		layout.Center = withoutWidget(layout.Center, disposable)
		layout.Right = withoutWidget(layout.Right, disposable)
	}
	left, center, right := b.layoutRegionsLocked(layout)
	left = ansi.Truncate(left, width, "")
	right = ansi.Truncate(right, width, "")
	center = ansi.Truncate(center, width, "")
	lw, rw, cw := ansi.StringWidth(left), ansi.StringWidth(right), ansi.StringWidth(center)
	if center == "" {
		return fillStatusWidth(left, "", right, width)
	}
	if lw+rw+cw+4 > width {
		compact := ansi.Truncate(joinWidgets(left, center, right), width, "")
		return compact + strings.Repeat(" ", max(0, width-ansi.StringWidth(compact)))
	}
	centerStart := (width - cw) / 2
	if centerStart < lw+2 {
		centerStart = lw + 2
	}
	rightStart := width - rw
	if centerStart+cw+2 > rightStart {
		centerStart = rightStart - cw - 2
	}
	return left + strings.Repeat(" ", centerStart-lw) + center + strings.Repeat(" ", rightStart-centerStart-cw) + right
}

func (b *Bar) layoutRegionsLocked(layout Layout) (left, center, right string) {
	left = b.regionLocked(layout.Left)
	center = b.regionLocked(layout.Center)
	right = b.regionLocked(layout.Right)
	if b.localVersion != "" {
		remote := b.remoteVersion
		if remote == "" {
			remote = "?"
		}
		right = joinWidgets(right, "pb "+b.localVersion+" / host "+remote)
	}
	if b.transport != "" {
		right = joinWidgets(right, b.semanticLocked(b.transport, "accent"))
	}
	if activity := b.activityLocked(); activity != "" && !containsWidget(layout.Left, "activity") && !containsWidget(layout.Center, "activity") && !containsWidget(layout.Right, "activity") {
		center = joinWidgets(center, activity)
	}
	return left, center, right
}

func withoutWidget(widgets []string, unwanted string) []string {
	result := widgets[:0]
	for _, widget := range widgets {
		if widget != unwanted {
			result = append(result, widget)
		}
	}
	return result
}

func (b *Bar) regionLocked(widgets []string) string {
	values := make([]string, 0, len(widgets))
	for _, widget := range widgets {
		if value := b.widgetLocked(widget); value != "" {
			values = append(values, value)
		}
	}
	return strings.Join(values, "  ")
}

func (b *Bar) widgetLocked(widget string) string {
	project, session, connection := b.identityLocked()
	switch widget {
	case "project":
		if b.privacy {
			return ""
		}
		return b.semanticLocked(project, "accent")
	case "session":
		if b.privacy {
			return ""
		}
		return session
	case "connection":
		semantic := "accent"
		if connection == "failed" || connection == "disconnected" {
			semantic = "error"
		} else if connection == "connecting" || connection == "reconnecting" {
			semantic = "warning"
		}
		return b.semanticLocked(connection, semantic)
	case "activity":
		return b.activityLocked()
	case "config_sync":
		if b.configSync == "" {
			return ""
		}
		return "sync " + b.configSync
	case "credits":
		if b.privacy || b.credits == "" {
			return ""
		}
		return "credits " + b.credits
	case "storage":
		if b.privacy || b.storage == "" {
			return ""
		}
		return "storage " + b.storage
	default:
		return ""
	}
}

func (b *Bar) activityLocked() string {
	if failure := b.currentFailureLocked(); failure != "" {
		return b.semanticLocked("! "+failure, "error")
	}
	if b.noticeActiveLocked() {
		if b.loading {
			return string("|/-\\"[b.spinner]) + " " + b.notice
		}
		return b.notice
	}
	return ""
}

func (b *Bar) barStyleLocked() (style, restore string) {
	if b.noColor || b.theme == ThemeMono || (b.theme == ThemeTerminal && !b.hasColorOverridesLocked()) || b.appSGROverflow {
		restore = "\x1b[27m"
		if b.appInverse {
			restore = "\x1b[7m"
		}
		return "\x1b[7m", restore
	}
	style = b.baseStyleLocked()
	restore = "\x1b[0m" + string(b.appSGR)
	return style, restore
}

func (b *Bar) baseStyleLocked() string {
	if b.theme == ThemeTerminal {
		if b.colors.Foreground == "" && b.colors.Background == "" {
			return "\x1b[39m\x1b[49m\x1b[7m"
		}
		return colorSequence(firstNonEmpty(b.colors.Foreground, "default"), false) + colorSequence(firstNonEmpty(b.colors.Background, "default"), true)
	}
	foreground, background := "bright_white", "black"
	if b.theme == ThemeLight {
		foreground, background = "black", "bright_white"
	}
	if b.colors.Foreground != "" {
		foreground = b.colors.Foreground
	}
	if b.colors.Background != "" {
		background = b.colors.Background
	}
	return colorSequence(foreground, false) + colorSequence(background, true)
}

func (b *Bar) semanticLocked(value, semantic string) string {
	if value == "" || b.noColor || b.theme == ThemeMono || (b.theme == ThemeTerminal && !b.hasColorOverridesLocked()) {
		return value
	}
	color := ""
	switch semantic {
	case "accent":
		color = firstNonEmpty(b.colors.Accent, "bright_cyan")
	case "warning":
		color = firstNonEmpty(b.colors.Warning, "bright_yellow")
	case "error":
		color = firstNonEmpty(b.colors.Error, "bright_red")
	}
	if color == "" {
		return value
	}
	return colorSequence(color, false) + value + b.baseStyleLocked()
}

func (b *Bar) hasColorOverridesLocked() bool {
	return b.colors.Foreground != "" || b.colors.Background != "" || b.colors.Accent != "" || b.colors.Warning != "" || b.colors.Error != ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func colorSequence(value string, background bool) string {
	value = strings.ToLower(strings.TrimSpace(value))
	base := 30
	if background {
		base = 40
	}
	if value == "" || value == "default" {
		if background {
			return "\x1b[49m"
		}
		return "\x1b[39m"
	}
	if strings.HasPrefix(value, "#") && len(value) == 7 {
		r, _ := strconv.ParseUint(value[1:3], 16, 8)
		g, _ := strconv.ParseUint(value[3:5], 16, 8)
		bl, _ := strconv.ParseUint(value[5:7], 16, 8)
		prefix := 38
		if background {
			prefix = 48
		}
		return fmt.Sprintf("\x1b[%d;2;%d;%d;%dm", prefix, r, g, bl)
	}
	names := []string{"black", "red", "green", "yellow", "blue", "magenta", "cyan", "white"}
	bright := strings.HasPrefix(value, "bright_")
	value = strings.TrimPrefix(value, "bright_")
	for index, name := range names {
		if value == name {
			if bright {
				base += 60
			}
			return fmt.Sprintf("\x1b[%dm", base+index)
		}
	}
	return ""
}

func containsWidget(widgets []string, wanted string) bool {
	for _, widget := range widgets {
		if widget == wanted {
			return true
		}
	}
	return false
}

func joinWidgets(parts ...string) string {
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			values = append(values, part)
		}
	}
	return strings.Join(values, "  ")
}

func fillStatusWidth(left, center, right string, width int) string {
	content := left
	if center != "" {
		content += " | " + center
	}
	content = ansi.Truncate(content, max(0, width-ansi.StringWidth(right)), "")
	padding := width - ansi.StringWidth(content) - ansi.StringWidth(right)
	if padding < 1 && right != "" {
		content = ansi.Truncate(content, max(0, width-ansi.StringWidth(right)-1), "")
		padding = 1
	}
	return content + strings.Repeat(" ", max(0, padding)) + right
}

func safeLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var out strings.Builder
	for _, r := range value {
		if !unicode.IsGraphic(r) || r == '\x1b' || r == '/' || r == '\\' {
			out.WriteRune(' ')
			continue
		}
		out.WriteRune(r)
		if out.Len() >= 80 {
			break
		}
	}
	return strings.Join(strings.Fields(out.String()), " ")
}
