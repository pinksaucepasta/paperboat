package prompt

import (
	"context"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/pinksaucepasta/paperboat/internal/selector"
)

type promptStyleSet struct {
	title lipgloss.Style
	help  lipgloss.Style
	err   lipgloss.Style
}

func promptStyles(ctx context.Context) promptStyleSet {
	return promptStyleSet{
		title: selector.TitleStyle(ctx),
		help:  selector.HelpStyle(ctx),
		err:   selector.ErrorStyle(ctx),
	}
}

func configureInputStyles(input *textinput.Model, ctx context.Context) {
	if input == nil {
		return
	}
	styles := input.Styles()
	accent := selector.AccentStyle(ctx)
	for _, state := range []*textinput.StyleState{&styles.Focused, &styles.Blurred} {
		state.Prompt = accent
		state.Placeholder = selector.HelpStyle(ctx)
	}
	styles.Cursor.Color = accent.GetForeground()
	input.SetStyles(styles)
}

func promptFooter(ctx context.Context, confirm, cancel string) string {
	keys := selector.HelpKeys(ctx)
	return fmt.Sprintf("%s %s  %s/%s %s", keys["select"], confirm, keys["back"], keys["interrupt"], cancel)
}

func promptWidth(width int) int {
	return max(1, width)
}

func promptLine(value string, width int) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(selector.SanitizeText(value), width, "...")
}

func promptInputWidth(width int) int {
	width = promptWidth(width)
	indent := min(2, width-1)
	return max(1, width-indent)
}

func promptInputView(value string, width int) string {
	indent := min(2, max(0, width-1))
	return ansi.Truncate(strings.Repeat(" ", indent)+value, width, "...")
}
