## pb config status-bar reset

Restore status-bar defaults

### Synopsis

Restore status-bar defaults

Restore the built-in status-bar preferences. This only changes local presentation and leaves account, terminal sessions, and remote configuration intact.

The status bar is rendered by the interactive terminal client. Preferences affect local presentation and do not change remote sessions. Use preview to inspect the result before relying on a theme or width setting.

JSON output is supported with --json.

```
pb config status-bar reset [flags]
```

### Options

```
  -h, --help   help for reset
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

