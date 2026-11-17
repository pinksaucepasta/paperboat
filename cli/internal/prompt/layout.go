package prompt

import (
	"context"
	"strings"

	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/pinksaucepasta/paperboat/cli/internal/selector"
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

func promptFooter(ctx context.Context, width int, confirm, cancel string) string {
	return selector.Footer(width, selector.Binding(ctx, "select", confirm), selector.Leave(ctx, cancel))
}

func promptWidth(width int) int {
	return max(1, width)
}

func promptLine(value string, width int) string {
	if width <= 0 {
		return ""
	}
	tail := "..."
	if width <= len(tail) {
		tail = "" // an ellipsis must never be wider than the screen
	}
	return ansi.Truncate(selector.SanitizeText(value), width, tail)
}

func promptInputWidth(width int) int {
	width = promptWidth(width)
	// textinput.View adds the "> " prompt and the cursor cell to its width.
	return max(1, width-min(2, width-1)-3)
}

func promptInputView(value string, width int) string {
	indent := min(2, max(0, width-1))
	return ansi.Truncate(strings.Repeat(" ", indent)+value, width, "...")
}
