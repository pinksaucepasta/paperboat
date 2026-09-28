## pb config set

Set a local configuration value

### Synopsis

Set a local configuration value

Write one supported local configuration key. The change affects this CLI installation; inspect pb config show afterward to confirm the effective value.

Configuration commands distinguish local CLI settings from synchronized repository content. Inspect current state before changing it; synchronization reports conflicts instead of silently overwriting one side. The --json option provides structured output for scripts where supported.

JSON output is supported with --json.

```
pb config set <key> <value> [flags]
```

### Options

```
  -h, --help   help for set
      --json   
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb config](pb_config.md)	 - Inspect the local CLI config

