package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/preferences"
	"github.com/pinksaucepasta/paperboat/internal/prompt"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func customizationPath(command *cobra.Command) (string, error) {
	return preferences.Path(configPathFlag(command))
}

func customizationCommand() *cobra.Command {
	root := &cobra.Command{Use: "customize", Short: "Customize local shortcuts, command defaults, and TUI appearance", Args: commandArgs(cobra.NoArgs), RunE: func(c *cobra.Command, _ []string) error {
		if jsonOutputRequested(c) {
			return showPreferences(c)
		}
		return editPreferences(c)
	}}
	root.Flags().Bool("json", false, "print local preferences instead of opening the editor")
	root.Long = `Customize local shortcuts, command defaults, themes, accent colors, keybindings,
list density, home order and visibility, favorite actions, columns and preview panels.
Changes stay in a draft until Save changes; leaving a changed draft offers discard.
Preferences are stored alongside the selected CLI config in a .preferences.json file.
They are local only and are not synchronized to the account.

Shortcuts invoke supported PB commands only. {1}, {2}, etc. substitute positional
arguments literally; {args} inserts remaining arguments as a whole template token.
There is no shell evaluation or recursive shortcut expansion. Built-in names are
reserved. Explicit flags override configured defaults. Use explain to inspect an
invocation without executing it. Use --no-customization to bypass preferences.

Themes are terminal, dark, light and mono. Arrow keys, Enter, Escape and Ctrl+C stay
available. Customize and All commands cannot be hidden. Preview URL, status, errors
and controls remain visible even when optional panels are hidden.

The editor validates changes and detects concurrent saves. Invalid preferences can
be repaired with import or reset; these commands remain available without loading
the invalid file. --json displays preferences without starting the editor.`
	root.Example = "  pb config customize\n  pb config customize path\n  pb config customize show --json\n  pb config customize explain -- mac -- uptime\n  pb config customize import ./preferences.json\n  pb config customize reset --yes"
	show := &cobra.Command{Use: "show", Short: "Show the local preference document", Args: commandArgs(cobra.NoArgs), RunE: func(c *cobra.Command, _ []string) error { return showPreferences(c) }}
	path := &cobra.Command{Use: "path", Short: "Print the local preference file path", Args: commandArgs(cobra.NoArgs), RunE: func(c *cobra.Command, _ []string) error {
		path, err := customizationPath(c)
		if err != nil {
			return err
		}
		if jsonOutputRequested(c) {
			return writeCLIJSON(c.OutOrStdout(), map[string]any{"path": path})
		}
		_, err = fmt.Fprintln(c.OutOrStdout(), path)
		return err
	}}
	validate := &cobra.Command{Use: "validate", Short: "Validate preferences without executing any action", Args: commandArgs(cobra.NoArgs), RunE: func(c *cobra.Command, _ []string) error {
		path, err := customizationPath(c)
		if err != nil {
			return err
		}
		doc, err := preferences.Load(path)
		if err != nil {
			return err
		}
		if err = validatePreferencesCommands(newRootCommand(), doc); err != nil {
			return err
		}
		return preferenceResult(c, map[string]any{"valid": true, "path": path}, "Preferences are valid.")
	}}
	importCmd := &cobra.Command{Use: "import <file>", Short: "Validate and replace local preferences from a JSON file", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(c *cobra.Command, args []string) error {
		path, err := customizationPath(c)
		if err != nil {
			return err
		}
		revision, err := preferences.Revision(path)
		if err != nil {
			return err
		}
		doc, err := preferences.Load(args[0])
		if err != nil {
			return err
		}
		if _, err = os.Stat(args[0]); err != nil {
			return err
		}
		if err = validatePreferencesCommands(newRootCommand(), doc); err != nil {
			return err
		}
		if err = preferences.SaveIfUnchanged(path, doc, revision); err != nil {
			return err
		}
		return preferenceResult(c, map[string]any{"saved": true, "path": path}, "Local preferences saved.")
	}}
	reset := &cobra.Command{Use: "reset", Short: "Reset only local CLI preferences; keep account and connection settings", Args: commandArgs(cobra.NoArgs), RunE: func(c *cobra.Command, _ []string) error {
		yes, _ := c.Flags().GetBool("yes")
		if !yes {
			return invocationError(errors.New("reset requires --yes; account and connection settings are preserved"))
		}
		path, err := customizationPath(c)
		if err != nil {
			return err
		}
		revision, err := preferences.Revision(path)
		if err != nil {
			return err
		}
		if err = preferences.SaveIfUnchanged(path, preferences.Default(), revision); err != nil {
			return err
		}
		return preferenceResult(c, map[string]any{"reset": true, "path": path}, "Local preferences reset.")
	}}
	reset.Flags().Bool("yes", false, "confirm resetting local preferences")
	explain := &cobra.Command{Use: "explain -- <arguments...>", Short: "Show command expansion without executing it", Args: commandArgs(cobra.MinimumNArgs(1)), RunE: func(c *cobra.Command, args []string) error {
		// Only use explicit input arguments; resolution performs no command execution.
		root := newRootCommand()
		path, err := customizationPath(c)
		if err != nil {
			return err
		}
		doc, err := preferences.Load(path)
		if err != nil {
			return err
		}
		if err = validatePreferencesCommands(root, doc); err != nil {
			return err
		}
		resolution, err := explainPreferences(root, doc, interactiveArgs(c, args))
		if err != nil {
			return err
		}
		return preferenceResult(c, map[string]any{"argv": resolution.Arguments, "executes": false, "shortcut": resolution.Shortcut, "defaults": resolution.Defaults}, "pb "+formatPreferenceArgs(resolution.Arguments))
	}}
	for _, child := range []*cobra.Command{show, path, validate, importCmd, reset, explain} {
		child.Flags().Bool("json", false, "print machine-readable JSON")
		root.AddCommand(child)
	}
	return root
}

func preferenceResult(c *cobra.Command, data any, message string) error {
	if jsonOutputRequested(c) {
		return writeCLIJSON(c.OutOrStdout(), data)
	}
	_, err := fmt.Fprintln(c.OutOrStdout(), message)
	return err
}
func showPreferences(c *cobra.Command) error {
	path, err := customizationPath(c)
	if err != nil {
		return err
	}
	doc, err := preferences.Load(path)
	if err != nil {
		return err
	}
	if err = validatePreferencesCommands(newRootCommand(), doc); err != nil {
		return err
	}
	if jsonOutputRequested(c) {
		return writeCLIJSON(c.OutOrStdout(), map[string]any{"path": path, "scope": "local", "preferences": doc})
	}
	enc := json.NewEncoder(c.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}
func formatPreferenceArgs(args []string) string {
	out := make([]string, len(args))
	for i, v := range args {
		if v != "" && !strings.ContainsAny(v, " \t\r\n\"'\\$`;&|<>()*?[]{}!#~") {
			out[i] = v
		} else {
			out[i] = "'" + strings.ReplaceAll(v, "'", "'\"'\"'") + "'"
		}
	}
	return strings.Join(out, " ")
}
func preferenceText(c *cobra.Command, title, description, initial string, validate func(string) error) (string, error) {
	return prompt.Text(prompt.TextOptions{Title: title, Description: description, Initial: initial, Stdin: os.Stdin, Output: c.ErrOrStderr(), Context: c.Context(), Validate: validate})
}
func preferenceConfirm(c *cobra.Command, title, description string) (bool, error) {
	return prompt.Confirm(prompt.ConfirmOptions{Title: title, Description: description, Stdin: os.Stdin, Output: c.ErrOrStderr(), Context: c.Context()})
}
func preferencePick(c *cobra.Command, title string, values []string, current string) (string, error) {
	items := make([]selector.Item, 0, len(values))
	for _, value := range values {
		description := ""
		if value == current {
			description = "Current selection"
		}
		items = append(items, selector.Item{ID: value, Title: value, Description: description})
	}
	choice, err := chooseHomeAction(c, title, items)
	return choice.ID, err
}
func preferenceDirty(a, b preferences.Document) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return !bytes.Equal(x, y)
}

func editPreferences(c *cobra.Command) error {
	if err := selector.RequireTerminal(os.Stdin); err != nil {
		return err
	}
	path, err := customizationPath(c)
	if err != nil {
		return err
	}
	revision, err := preferences.Revision(path)
	if err != nil {
		return err
	}
	original, loadErr := preferences.Load(path)
	if loadErr == nil {
		loadErr = validatePreferencesCommands(newRootCommand(), original)
	}
	if loadErr != nil {
		if err = showHomeText(c, "Preferences need attention", loadErr.Error()+"\n\nThe original file has not changed. You can start a new draft from defaults, repair the file, or use --no-customization."); err != nil {
			return err
		}
		yes, err := preferenceConfirm(c, "Start a draft from defaults?", "Only Save changes will replace the invalid preference file.")
		if err != nil || !yes {
			return err
		}
		original = preferences.Default()
	}
	draft := preferences.Clone(original)
	parentCtx := c.Context()
	saved := false
	defer func() {
		if saved {
			c.SetContext(preferences.WithContext(parentCtx, draft))
		} else {
			c.SetContext(parentCtx)
		}
	}()
	end := selector.BeginScreen(c.ErrOrStderr())
	defer end()
	notice := "Changes stay in this draft until you save."
	for {
		c.SetContext(preferences.WithContext(parentCtx, draft))
		state := "No unsaved changes"
		if preferenceDirty(original, draft) || loadErr != nil {
			state = "Unsaved changes"
		}
		choice, err := selector.Choose(selector.Options{Context: c.Context(), Title: "Make Paperboat yours", Subtitle: state + " · local to this device", Header: "PAPERBOAT / CUSTOMIZE", Stdin: os.Stdin, Output: c.ErrOrStderr(), Footer: notice + "  ·  esc back", Items: []selector.Item{
			{ID: "shortcuts", Title: "Shortcuts", Description: fmt.Sprintf("%d shortcuts · connect, SSH, file tools, and other Paperboat actions", len(draft.Shortcuts))},
			{ID: "ports", Title: "Port shortcuts", Description: "pb 3000 → " + draft.PortAction},
			{ID: "defaults", Title: "Command defaults", Description: fmt.Sprintf("%d commands customized · explicit flags always win", len(draft.Defaults))},
			{ID: "appearance", Title: "Appearance", Description: draft.TUI.Theme + " theme · " + draft.TUI.Density + " lists"},
			{ID: "keys", Title: "Keybindings", Description: "Navigation and preview controls · Enter, Esc and Ctrl+C remain available"},
			{ID: "home", Title: "Home layout", Description: "Reorder or hide sections; All commands and Customize always remain"},
			{ID: "favorites", Title: "Favorite actions", Description: "Pin your shortcuts to the home screen"},
			{ID: "fields", Title: "Columns & panels", Description: "Choose and reorder built-in list details and preview panels"},
			{ID: "preview", Title: "Preview your home", Description: "Try the draft layout without running any commands"},
			{ID: "review", Title: "Review configuration", Description: path},
			{ID: "save", Title: "Save changes", Description: "Validate and atomically save this local configuration", Action: true},
			{ID: "discard", Title: "Discard and return", Description: "Leave the saved preferences untouched"},
		}})
		if interactiveCanceled(err) || errors.Is(err, selector.ErrInterrupted) {
			choice.ID = "discard"
			err = nil
		}
		if err != nil {
			return err
		}
		switch choice.ID {
		case "shortcuts":
			err = editPreferenceShortcuts(c, &draft)
		case "ports":
			var value string
			value, err = preferencePick(c, "When you type a port number", []string{"preview", "tunnel"}, draft.PortAction)
			if err == nil {
				draft.PortAction = value
			}
		case "defaults":
			err = editPreferenceDefaults(c, &draft)
		case "appearance":
			err = editPreferenceAppearance(c, &draft)
		case "keys":
			err = editPreferenceKeys(c, &draft)
		case "home":
			base := homeItems()
			all := make([]string, 0, len(base))
			titles := map[string]string{}
			for _, item := range base {
				all = append(all, item.ID)
				titles[item.ID] = item.Title
			}
			selected := orderedPreferenceIDs(all, draft.TUI.HomeOrder, draft.TUI.HomeHidden)
			selected, err = editPreferenceOrder(c, "Home sections", all, titles, selected, []string{"commands", "customize"})
			if err == nil {
				draft.TUI.HomeOrder = selected
				draft.TUI.HomeHidden = []string{}
				for _, id := range all {
					if !slices.Contains(selected, id) {
						draft.TUI.HomeHidden = append(draft.TUI.HomeHidden, id)
					}
				}
			}
		case "favorites":
			names := sortedPreferenceNames(draft.Shortcuts)
			if len(names) == 0 {
				err = showHomeText(c, "Favorite actions", "Create a shortcut first, then pin it here.")
			} else {
				draft.TUI.Favorites, err = editPreferenceOrder(c, "Favorite actions", names, nil, draft.TUI.Favorites, nil)
			}
		case "fields":
			err = editPreferenceFields(c, &draft)
		case "preview":
			_, err = selector.Choose(selector.Options{Context: c.Context(), Title: "Home preview", Subtitle: "Preview only · selecting an item returns to the editor", Items: personalizedHomeItems(draft), Stdin: os.Stdin, Output: c.ErrOrStderr()})
		case "review":
			data, _ := json.MarshalIndent(draft, "", "  ")
			err = showHomeText(c, "Local preference draft", string(data))
		case "save":
			if err = preferences.Validate(draft); err == nil {
				err = validatePreferencesCommands(newRootCommand(), draft)
			}
			if err == nil {
				err = preferences.SaveIfUnchanged(path, draft, revision)
			}
			if err == nil {
				saved = true
				return nil
			}
		case "discard":
			if !preferenceDirty(original, draft) && loadErr == nil {
				return nil
			}
			var yes bool
			yes, err = preferenceConfirm(c, "Discard unsaved changes?", "Your saved preferences will remain unchanged.")
			if err == nil && yes {
				return nil
			}
		}
		if err != nil && !interactiveCanceled(err) {
			if displayErr := showHomeFailure(c, err); displayErr != nil {
				return displayErr
			}
		}
	}
}

func sortedPreferenceNames[T any](values map[string]T) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
func orderedPreferenceIDs(all, order, hidden []string) []string {
	out := []string{}
	for _, id := range append(append([]string{}, order...), all...) {
		if slices.Contains(all, id) && !slices.Contains(hidden, id) && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// Each row opens explicit move/show/hide actions so keyboard and mouse users
// share the same layout editor. The saved list contains only enabled fields.
func editPreferenceOrder(c *cobra.Command, title string, all []string, labels map[string]string, selected, mandatory []string) ([]string, error) {
	current := append([]string{}, selected...)
	for {
		items := []selector.Item{{ID: "done", Title: "Done", Description: "Keep this layout in the draft", Action: true}}
		for _, id := range append(append([]string{}, current...), all...) {
			if slices.ContainsFunc(items, func(item selector.Item) bool { return item.ID == id }) {
				continue
			}
			label := labels[id]
			if label == "" {
				label = id
			}
			description := "Hidden"
			if index := slices.Index(current, id); index >= 0 {
				description = fmt.Sprintf("Visible · position %d", index+1)
			}
			if slices.Contains(mandatory, id) {
				description += " · always available"
			}
			items = append(items, selector.Item{ID: id, Title: label, Description: description})
		}
		choice, err := chooseHomeAction(c, title, items)
		if interactiveCanceled(err) {
			return current, nil
		}
		if err != nil {
			return selected, err
		}
		if choice.ID == "done" {
			return current, nil
		}
		index := slices.Index(current, choice.ID)
		actions := []selector.Item{}
		if index < 0 {
			actions = append(actions, selector.Item{ID: "show", Title: "Show"})
		} else {
			if index > 0 {
				actions = append(actions, selector.Item{ID: "up", Title: "Move up"})
			}
			if index < len(current)-1 {
				actions = append(actions, selector.Item{ID: "down", Title: "Move down"})
			}
			if !slices.Contains(mandatory, choice.ID) {
				actions = append(actions, selector.Item{ID: "hide", Title: "Hide"})
			}
		}
		if len(actions) == 0 {
			continue
		}
		action, err := chooseHomeAction(c, choice.Title, actions)
		if interactiveCanceled(err) {
			continue
		}
		if err != nil {
			return selected, err
		}
		switch action.ID {
		case "show":
			current = append(current, choice.ID)
		case "hide":
			current = append(current[:index], current[index+1:]...)
		case "up":
			current[index], current[index-1] = current[index-1], current[index]
		case "down":
			current[index], current[index+1] = current[index+1], current[index]
		}
	}
}

func editPreferenceAppearance(c *cobra.Command, doc *preferences.Document) error {
	baseCtx := c.Context()
	for {
		c.SetContext(preferences.WithContext(baseCtx, *doc))
		item, err := chooseHomeAction(c, "Appearance", []selector.Item{{ID: "theme", Title: "Theme", Description: doc.TUI.Theme}, {ID: "accent", Title: "Accent color", Description: orNone(doc.TUI.Accent)}, {ID: "density", Title: "List density", Description: doc.TUI.Density}, {ID: "done", Title: "Done", Action: true}})
		if err != nil {
			return err
		}
		switch item.ID {
		case "done":
			return nil
		case "theme":
			v, e := preferencePick(c, "Theme", []string{"terminal", "dark", "light", "mono"}, doc.TUI.Theme)
			if e == nil {
				doc.TUI.Theme = v
			}
			err = e
		case "density":
			v, e := preferencePick(c, "List density", []string{"comfortable", "compact"}, doc.TUI.Density)
			if e == nil {
				doc.TUI.Density = v
			}
			err = e
		case "accent":
			v, e := preferenceText(c, "Accent color", "#RRGGBB or an ANSI color number; leave empty for the theme default", doc.TUI.Accent, func(value string) error {
				copy := preferences.Clone(*doc)
				copy.TUI.Accent = value
				return preferences.Validate(copy)
			})
			if e == nil {
				doc.TUI.Accent = v
			}
			err = e
		}
		if err != nil && !interactiveCanceled(err) {
			return err
		}
	}
}

func editPreferenceKeys(c *cobra.Command, doc *preferences.Document) error {
	baseCtx := c.Context()
	names := []string{"up", "down", "select", "back", "preview_open", "preview_background", "preview_tunnel", "preview_stop"}
	for {
		items := []selector.Item{{ID: "done", Title: "Done", Action: true}}
		for _, name := range names {
			items = append(items, selector.Item{ID: name, Title: strings.ReplaceAll(name, "_", " "), Description: doc.TUI.Keys[name]})
		}
		item, err := chooseHomeAction(c, "Keybindings", items)
		if err != nil {
			return err
		}
		if item.ID == "done" {
			return nil
		}
		value, err := preferenceText(c, "Key for "+item.Title, "Use e.g. ctrl+j, alt+k, f2; preview actions also accept letters. Enter/Esc/Ctrl+C stay reserved.", doc.TUI.Keys[item.ID], func(value string) error {
			copy := preferences.Clone(*doc)
			if copy.TUI.Keys == nil {
				copy.TUI.Keys = map[string]string{}
			}
			copy.TUI.Keys[item.ID] = value
			return preferences.Validate(copy)
		})
		if interactiveCanceled(err) {
			continue
		}
		if err != nil {
			return err
		}
		if doc.TUI.Keys == nil {
			doc.TUI.Keys = map[string]string{}
		}
		doc.TUI.Keys[item.ID] = value
		c.SetContext(preferences.WithContext(baseCtx, *doc))
	}
}

var preferenceColumnCatalog = map[string][]string{"machines": preferences.ColumnIDs("machines"), "sessions": preferences.ColumnIDs("sessions"), "previews": preferences.ColumnIDs("previews"), "tunnels": preferences.ColumnIDs("tunnels")}

func editPreferenceFields(c *cobra.Command, doc *preferences.Document) error {
	for {
		choice, err := chooseHomeAction(c, "Columns & panels", []selector.Item{{ID: "machines", Title: "Machine details"}, {ID: "sessions", Title: "Session details"}, {ID: "previews", Title: "Preview list details"}, {ID: "tunnels", Title: "Tunnel list details"}, {ID: "panels", Title: "Live preview panels", Description: "URL, status, errors, and controls always remain visible"}, {ID: "done", Title: "Done", Action: true}})
		if err != nil {
			return err
		}
		if choice.ID == "done" {
			return nil
		}
		if choice.ID == "panels" {
			doc.TUI.PreviewPanels, err = editPreferenceOrder(c, "Live preview panels", preferences.PreviewPanelIDs(), nil, doc.TUI.PreviewPanels, nil)
		} else {
			if doc.TUI.Columns == nil {
				doc.TUI.Columns = map[string][]string{}
			}
			all := preferenceColumnCatalog[choice.ID]
			selected := doc.TUI.Columns[choice.ID]
			if selected == nil {
				selected = preferences.Default().TUI.Columns[choice.ID]
			}
			doc.TUI.Columns[choice.ID], err = editPreferenceOrder(c, choice.Title, all, nil, selected, nil)
		}
		if err != nil && !interactiveCanceled(err) {
			return err
		}
	}
}

func editPreferenceShortcuts(c *cobra.Command, doc *preferences.Document) error {
	for {
		items := []selector.Item{{ID: "+", Title: "New shortcut", Description: "Guided machine actions or any supported Paperboat command", Action: true}}
		for _, name := range sortedPreferenceNames(doc.Shortcuts) {
			s := doc.Shortcuts[name]
			items = append(items, selector.Item{ID: name, Title: "pb " + name, Description: "pb " + formatPreferenceArgs(append(append([]string{}, s.Command...), s.Args...))})
		}
		item, err := chooseHomeAction(c, "Shortcuts", items)
		if err != nil {
			return err
		}
		if item.ID == "+" {
			err = addPreferenceShortcut(c, doc)
		} else {
			action, e := chooseHomeAction(c, item.Title, []selector.Item{{ID: "edit", Title: "Edit argument template"}, {ID: "favorite", Title: "Toggle home favorite"}, {ID: "remove", Title: "Remove shortcut"}})
			if interactiveCanceled(e) {
				continue
			}
			if e != nil {
				return e
			}
			switch action.ID {
			case "favorite":
				if i := slices.Index(doc.TUI.Favorites, item.ID); i >= 0 {
					doc.TUI.Favorites = append(doc.TUI.Favorites[:i], doc.TUI.Favorites[i+1:]...)
				} else {
					doc.TUI.Favorites = append(doc.TUI.Favorites, item.ID)
				}
			case "remove":
				yes, e := preferenceConfirm(c, "Remove "+item.Title+"?", "This removes only the local shortcut.")
				err = e
				if e == nil && yes {
					delete(doc.Shortcuts, item.ID)
					doc.TUI.Favorites = slices.DeleteFunc(doc.TUI.Favorites, func(v string) bool { return v == item.ID })
				}
			case "edit":
				s := doc.Shortcuts[item.ID]
				var argv []string
				argv, err = editPreferenceTemplate(c, *doc, item.ID, s.Command, s.Args)
				if err == nil {
					s.Args = argv
					doc.Shortcuts[item.ID] = s
				}
			}
		}
		if err != nil && !interactiveCanceled(err) {
			if e := showHomeFailure(c, err); e != nil {
				return e
			}
		}
	}
}

func addPreferenceShortcut(c *cobra.Command, doc *preferences.Document) error {
	name, err := preferenceText(c, "Shortcut name", "For example mac, upload, or dev; built-in commands and numbers are reserved", "", func(name string) error {
		candidate := preferences.Clone(*doc)
		if candidate.Shortcuts == nil {
			candidate.Shortcuts = map[string]preferences.Shortcut{}
		}
		if _, ok := candidate.Shortcuts[name]; ok {
			return errors.New("a shortcut already uses this name")
		}
		candidate.Shortcuts[name] = preferences.Shortcut{Command: []string{"connect"}, Args: []string{"{args}"}}
		if err := preferences.Validate(candidate); err != nil {
			return err
		}
		return validatePreferencesCommands(newRootCommand(), candidate)
	})
	if err != nil {
		return err
	}
	kind, err := chooseHomeAction(c, "What should pb "+name+" do?", []selector.Item{{ID: "connect", Title: "Paperboat terminal"}, {ID: "ssh", Title: "SSH"}, {ID: "sftp", Title: "SFTP"}, {ID: "scp-upload", Title: "SCP upload", Description: "pb shortcut LOCAL REMOTE_PATH"}, {ID: "scp-download", Title: "SCP download", Description: "pb shortcut REMOTE_PATH LOCAL"}, {ID: "rsync-upload", Title: "Rsync upload", Description: "pb shortcut LOCAL REMOTE_PATH"}, {ID: "rsync-download", Title: "Rsync download", Description: "pb shortcut REMOTE_PATH LOCAL"}, {ID: "command", Title: "Choose another Paperboat command"}})
	if err != nil {
		return err
	}
	var command, argv []string
	if kind.ID == "command" {
		choice, e := chooseHomeAction(c, "Shortcut command", preferenceCommandItems())
		if e != nil {
			return e
		}
		command = strings.Fields(choice.ID)
		argv, err = editPreferenceTemplate(c, *doc, name, command, []string{"{args}"})
		if err != nil {
			return err
		}
	} else {
		device, e := preferenceText(c, "Machine name or ID", "Use the machine name you normally pass to pb; it will be resolved when the shortcut runs", "", func(value string) error {
			if value == "" || strings.ContainsAny(value, "{}\r\n\t:") || strings.HasPrefix(value, "-") {
				return errors.New("enter a machine name or ID without template markers or a path")
			}
			return nil
		})
		if e != nil {
			return e
		}
		operation, mode, _ := strings.Cut(kind.ID, "-")
		command = []string{operation}
		argv = []string{device, "{args}"}
		if operation == "sftp" {
			argv = []string{"{args}", device}
		}
		if mode == "upload" {
			argv = []string{"{args}", "{1}", device + ":{2}"}
		}
		if mode == "download" {
			argv = []string{"{args}", device + ":{1}", "{2}"}
		}
	}
	candidate := preferences.Clone(*doc)
	if candidate.Shortcuts == nil {
		candidate.Shortcuts = map[string]preferences.Shortcut{}
	}
	candidate.Shortcuts[name] = preferences.Shortcut{Command: command, Args: argv}
	if err := preferences.Validate(candidate); err != nil {
		return err
	}
	if err := validatePreferencesCommands(newRootCommand(), candidate); err != nil {
		return err
	}
	yes, err := preferenceConfirm(c, "Add pb "+name+"?", "Expands to pb "+formatPreferenceArgs(append(append([]string{}, command...), argv...))+". Nothing runs until you invoke the shortcut.")
	if err != nil {
		return err
	}
	if yes {
		*doc = candidate
	}
	return nil
}
func editPreferenceTemplate(c *cobra.Command, doc preferences.Document, name string, command, initial []string) ([]string, error) {
	var parsed []string
	_, err := preferenceText(c, "Arguments for pb "+name, "Use {1}, {2} for supplied arguments and {args} for the rest. Quotes group arguments; no shell expansion.", formatPreferenceArgs(initial), func(value string) error {
		var err error
		parsed, err = splitPreferenceArgs(value)
		if err != nil {
			return errors.New("check the argument quoting")
		}
		candidate := preferences.Clone(doc)
		if candidate.Shortcuts == nil {
			candidate.Shortcuts = map[string]preferences.Shortcut{}
		}
		candidate.Shortcuts[name] = preferences.Shortcut{Command: command, Args: parsed}
		if err = preferences.Validate(candidate); err != nil {
			return err
		}
		return validatePreferencesCommands(newRootCommand(), candidate)
	})
	return parsed, err
}
func preferenceCommandItems() []selector.Item {
	root := newRootCommand()
	items := interactiveCommandItems(root, nil)
	return slices.DeleteFunc(items, func(item selector.Item) bool {
		doc := preferences.Default()
		doc.Shortcuts = map[string]preferences.Shortcut{"sample-shortcut": {Command: strings.Fields(item.ID), Args: []string{"{args}"}}}
		return validatePreferencesCommands(root, doc) != nil
	})
}
func editPreferenceDefaults(c *cobra.Command, doc *preferences.Document) error {
	for {
		items := []selector.Item{{ID: "+", Title: "Add command defaults", Description: "Choose a command and a configurable flag", Action: true}}
		for _, name := range sortedPreferenceNames(doc.Defaults) {
			items = append(items, selector.Item{ID: name, Title: "pb " + name, Description: fmt.Sprintf("%d flag defaults", len(doc.Defaults[name]))})
		}
		item, err := chooseHomeAction(c, "Command defaults", items)
		if err != nil {
			return err
		}
		path := item.ID
		if path == "+" {
			choice, e := chooseHomeAction(c, "Choose a command", preferenceCommandItems())
			if e != nil {
				if interactiveCanceled(e) {
					continue
				}
				return e
			}
			path = choice.ID
		}
		root := newRootCommand()
		target, _, e := root.Find(strings.Fields(path))
		if e != nil {
			return e
		}
		target.InheritedFlags()
		flags := []selector.Item{}
		target.Flags().VisitAll(func(flag *pflag.Flag) {
			candidate := preferences.Clone(*doc)
			if candidate.Defaults == nil {
				candidate.Defaults = map[string]map[string]string{}
			}
			candidate.Defaults[path] = map[string]string{flag.Name: flag.DefValue}
			if validatePreferencesCommands(root, candidate) == nil {
				value := flag.DefValue
				if configured, ok := doc.Defaults[path][flag.Name]; ok {
					value = configured
				}
				flags = append(flags, selector.Item{ID: flag.Name, Title: "--" + flag.Name, Description: flag.Usage + " · current " + value})
			}
		})
		for _, name := range sortedPreferenceNames(doc.Defaults[path]) {
			flags = append(flags, selector.Item{ID: "remove:" + name, Title: "Reset --" + name, Description: "Use the built-in default again"})
		}
		if len(flags) == 0 {
			if err = showHomeText(c, "No configurable defaults", "This command has no supported default flags. Authentication, confirmations, secrets and output controls are set explicitly."); err != nil {
				return err
			}
			continue
		}
		selected, e := chooseHomeAction(c, "Defaults for pb "+path, flags)
		if interactiveCanceled(e) {
			continue
		}
		if e != nil {
			return e
		}
		if strings.HasPrefix(selected.ID, "remove:") {
			delete(doc.Defaults[path], strings.TrimPrefix(selected.ID, "remove:"))
			if len(doc.Defaults[path]) == 0 {
				delete(doc.Defaults, path)
			}
			continue
		}
		flag := target.Flags().Lookup(selected.ID)
		initial := flag.DefValue
		if configured, ok := doc.Defaults[path][flag.Name]; ok {
			initial = configured
		}
		value, e := preferenceText(c, "Default --"+flag.Name, flag.Usage+". Explicit command flags override this value.", initial, func(value string) error {
			candidate := preferences.Clone(*doc)
			if candidate.Defaults == nil {
				candidate.Defaults = map[string]map[string]string{}
			}
			candidate.Defaults[path] = map[string]string{flag.Name: value}
			return validatePreferencesCommands(newRootCommand(), candidate)
		})
		if interactiveCanceled(e) {
			continue
		}
		if e != nil {
			return e
		}
		if doc.Defaults == nil {
			doc.Defaults = map[string]map[string]string{}
		}
		if doc.Defaults[path] == nil {
			doc.Defaults[path] = map[string]string{}
		}
		doc.Defaults[path][flag.Name] = value
	}
}
