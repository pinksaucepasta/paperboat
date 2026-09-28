## pb config status-bar show

Show the effective status-bar configuration

### Synopsis

Show the effective status-bar configuration

Show the effective status-bar settings, including defaults and local overrides. This is read-only and does not attach to a session.

The status bar is rendered by the interactive terminal client. Preferences affect local presentation and do not change remote sessions. Use preview to inspect the result before relying on a theme or width setting.

JSON output is supported with --json.

```
pb config status-bar show [flags]
```

### Options

```
  -h, --help   help for show
      --json   print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb config status-bar](pb_config_status-bar.md)	 - Configure the interactive terminal status bar

