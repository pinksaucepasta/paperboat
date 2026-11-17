package selector

import (
	"context"
	"slices"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
)

// Binding is the one definition of a preference action's keys (its defaults plus the
// user's own): handlers match against it and footers are drawn from it, so a footer
// can only name keys that work.
func Binding(ctx context.Context, action, desc string) key.Binding {
	return withKeys(actionKeys(ctx, action), desc)
}

// Move binds every key that moves the selection up or down.
// Its label names the primary keys plus any the user added ("↑/↓", "↑/↓/Ctrl+U"); the
// built-in Ctrl+K/Ctrl+N aliases match without crowding the footer.
func Move(ctx context.Context, desc string) key.Binding {
	up, down := actionKeys(ctx, "up"), actionKeys(ctx, "down")
	label := joinKeyLabels(slices.Concat(up[:1], down[:1], up[2:], down[2:]))
	return key.NewBinding(key.WithKeys(slices.Concat(up, down)...), key.WithHelp(label, desc))
}

// Leave binds the keys that go back, plus Ctrl+C (which always interrupts).
func Leave(ctx context.Context, desc string) key.Binding {
	return withKeys(append(actionKeys(ctx, "back"), "ctrl+c"), desc)
}

// Extend adds keys no preference controls (y/n, PgUp) to a binding, keeping its help.
func Extend(binding key.Binding, desc string, keys ...string) key.Binding {
	return withKeys(slices.Concat(binding.Keys(), keys), desc)
}

// Hint is a fixed key such as PgUp/PgDn or typing, with its footer label.
func Hint(label, desc string, keys ...string) key.Binding {
	return key.NewBinding(key.WithKeys(keys...), key.WithHelp(label, desc))
}

func withKeys(keys []string, desc string) key.Binding {
	return key.NewBinding(key.WithKeys(keys...), key.WithHelp(joinKeyLabels(keys), desc))
}

// Footer renders bindings as one plain "key action" line that drops whole items
// (with an ellipsis) instead of cutting one in half; width <= 0 never drops. Callers
// style the line.
func Footer(width int, bindings ...key.Binding) string {
	h := help.New()
	h.Styles = help.Styles{}
	h.ShortSeparator = "  "
	h.SetWidth(width)
	return h.ShortHelpView(bindings)
}
