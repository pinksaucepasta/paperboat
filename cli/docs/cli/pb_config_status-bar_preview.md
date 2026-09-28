## pb config status-bar preview

Preview the configured status bar

### Synopsis

Preview the configured status bar

Render the current status bar at the requested width without opening a remote session. Use it to check truncation and theme choices before connecting.

The status bar is rendered by the interactive terminal client. Preferences affect local presentation and do not change remote sessions. Use preview to inspect the result before relying on a theme or width setting.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb config status-bar preview [flags]
```

### Options

```
  -h, --help        help for preview
      --width int   preview width (20-500 columns)
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb config status-bar](pb_config_status-bar.md)	 - Configure the interactive terminal status bar

