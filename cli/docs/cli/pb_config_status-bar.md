## pb config status-bar

Configure the interactive terminal status bar

### Synopsis

Configure the interactive terminal status bar

Inspect, set, preview, or reset local status-bar preferences. These settings affect interactive terminal display and do not modify remote shell state.

Configuration commands distinguish local CLI settings from synchronized repository content. Inspect current state before changing it; synchronization reports conflicts instead of silently overwriting one side. The --json option provides structured output for scripts where supported.

With --json, this command group lists its available commands.

```
pb config status-bar [flags]
```

### Options

```
  -h, --help   help for status-bar
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb config](pb_config.md)	 - Inspect the local CLI config
* [pb config status-bar preview](pb_config_status-bar_preview.md)	 - Preview the configured status bar
* [pb config status-bar reset](pb_config_status-bar_reset.md)	 - Restore status-bar defaults
* [pb config status-bar set](pb_config_status-bar_set.md)	 - Set a status-bar preference
* [pb config status-bar show](pb_config_status-bar_show.md)	 - Show the effective status-bar configuration

