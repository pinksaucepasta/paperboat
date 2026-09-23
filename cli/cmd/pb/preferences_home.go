package main

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/preferences"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/spf13/cobra"
)

func personalizedHomeItems(doc preferences.Document) []selector.Item {
	base := homeItems()
	ids := make([]string, 0, len(base))
	byID := map[string]selector.Item{}
	for _, item := range base {
		ids = append(ids, item.ID)
		byID[item.ID] = item
	}
	order := orderedPreferenceIDs(ids, doc.TUI.HomeOrder, doc.TUI.HomeHidden)
	result := make([]selector.Item, 0, len(base)+len(doc.TUI.Favorites))
	for _, name := range doc.TUI.Favorites {
		if shortcut, ok := doc.Shortcuts[name]; ok {
			result = append(result, selector.Item{ID: "shortcut:" + name, Title: "pb " + name, Description: "pb " + formatPreferenceArgs(append(append([]string{}, shortcut.Command...), shortcut.Args...)), Favorite: true})
		}
	}
	for _, id := range order {
		result = append(result, byID[id])
	}
	// Recovery and discovery never depend on a configurable menu row.
	for _, id := range []string{"customize", "commands"} {
		if !slices.Contains(order, id) {
			result = append(result, byID[id])
		}
	}
	return result
}

func preferenceDetails(ctx context.Context, resource string, values map[string]string) string {
	doc := preferences.FromContext(ctx)
	columns, exists := doc.TUI.Columns[resource]
	if !exists || columns == nil {
		columns = preferenceColumnCatalog[resource]
	}
	result := []string{}
	for _, column := range columns {
		if value := values[column]; value != "" {
			result = append(result, value)
		}
	}
	return strings.Join(result, " · ")
}

func runPreferenceFavorite(c *cobra.Command, name string) error {
	doc := preferences.FromContext(c.Context())
	shortcut, ok := doc.Shortcuts[name]
	if !ok {
		return errors.New("shortcut is no longer available; reopen Customize")
	}
	args := []string{name}
	needsInput := false
	for _, value := range shortcut.Args {
		if strings.Contains(value, "{") && value != "{args}" {
			needsInput = true
		}
	}
	// A command with only {args} may still require operands. Validate expansion
	// without invoking it; prompt if the canonical command needs arguments.
	if !needsInput {
		root := newRootCommand()
		resolution, err := explainPreferences(root, doc, args)
		if err != nil {
			return err
		}
		selected, remaining, err := root.Find(resolution.Arguments)
		if err == nil {
			err = selected.ParseFlags(remaining)
		}
		if err == nil {
			err = selected.ValidateArgs(selected.Flags().Args())
		}
		if err == nil {
			err = selected.ValidateRequiredFlags()
		}
		needsInput = err != nil
	}
	if needsInput {
		value, err := preferenceText(c, "Arguments for pb "+name, "Template: pb "+formatPreferenceArgs(append(append([]string{}, shortcut.Command...), shortcut.Args...)), "", func(value string) error {
			operands, err := splitPreferenceArgs(value)
			if err != nil {
				return errors.New("check the argument quoting")
			}
			root := newRootCommand()
			resolution, err := explainPreferences(root, doc, append([]string{name}, operands...))
			if err != nil {
				return err
			}
			target, remaining, err := root.Find(resolution.Arguments)
			if err != nil {
				return err
			}
			if err = target.ParseFlags(remaining); err != nil {
				return err
			}
			if err = target.ValidateArgs(target.Flags().Args()); err != nil {
				return err
			}
			return target.ValidateRequiredFlags()
		})
		if err != nil {
			return err
		}
		operands, _ := splitPreferenceArgs(value)
		args = append(args, operands...)
	}
	resolution, err := explainPreferences(newRootCommand(), doc, args)
	if err != nil {
		return err
	}

	if interactiveStreaming(resolution.Arguments) {
		return executeInteractiveCommand(c, args)
	}
	return runHomeResult(c, args)
}

func personalizedMachineCompletion(c *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	values, directive := machineCompletion(c, args, toComplete)
	if len(args) != 0 {
		return values, directive
	}
	doc := preferences.FromContext(c.Context())
	aliases := []string{}
	for _, name := range sortedPreferenceNames(doc.Shortcuts) {
		if strings.HasPrefix(name, toComplete) {
			aliases = append(aliases, name+"\tLocal shortcut for pb "+strings.Join(doc.Shortcuts[name].Command, " "))
			values = slices.DeleteFunc(values, func(v string) bool { return strings.SplitN(v, "\t", 2)[0] == name })
		}
	}
	return append(aliases, values...), directive
}
