## pb config status-bar set

Set a status-bar preference

### Synopsis

Set a status-bar preference

Set one status-bar preference by key and value. Preview the result before using it in a full-screen terminal application.

The status bar is rendered by the interactive terminal client. Preferences affect local presentation and do not change remote sessions. Use preview to inspect the result before relying on a theme or width setting.

JSON output is supported with --json.

```
pb config status-bar set <key> <value> [flags]
```

### Options

```
  -h, --help   help for set
      --json   print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb config status-bar](pb_config_status-bar.md)	 - Configure the interactive terminal status bar

