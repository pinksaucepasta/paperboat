## pb config customize

Customize local shortcuts, command defaults, and TUI appearance

### Synopsis

Customize local shortcuts, command defaults, themes, accent colors, keybindings,
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
the invalid file. --json displays preferences without starting the editor.

JSON output is supported with --json.

```
pb config customize [flags]
```

### Examples

```
  pb config customize
  pb config customize path
  pb config customize show --json
  pb config customize explain -- mac -- uptime
  pb config customize import ./preferences.json
  pb config customize reset --yes
```

### Options

```
  -h, --help   help for customize
      --json   print local preferences instead of opening the editor
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb config](pb_config.md)	 - Inspect the local CLI config
* [pb config customize explain](pb_config_customize_explain.md)	 - Show command expansion without executing it
* [pb config customize import](pb_config_customize_import.md)	 - Validate and replace local preferences from a JSON file
* [pb config customize path](pb_config_customize_path.md)	 - Print the local preference file path
* [pb config customize reset](pb_config_customize_reset.md)	 - Reset only local CLI preferences; keep account and connection settings
* [pb config customize show](pb_config_customize_show.md)	 - Show the local preference document
* [pb config customize validate](pb_config_customize_validate.md)	 - Validate preferences without executing any action

