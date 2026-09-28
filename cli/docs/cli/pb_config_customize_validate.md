## pb config customize validate

Validate preferences without executing any action

### Synopsis

Validate preferences without executing any action

Check the current preference document for invalid keys, values, and command expansions. Validation does not execute a shortcut or change preferences.

Customization is local to this CLI installation. It can change shortcuts, command defaults, and terminal appearance without changing account credentials or remote resources. Validate or explain preferences before applying an unfamiliar document.

JSON output is supported with --json.

```
pb config customize validate [flags]
```

### Options

```
  -h, --help   help for validate
      --json   print machine-readable JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb config customize](pb_config_customize.md)	 - Customize local shortcuts, command defaults, and TUI appearance

