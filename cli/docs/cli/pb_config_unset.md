## pb config unset

Remove a local configuration value

### Synopsis

Remove a local configuration value

Remove the supported local server setting so the built-in or surrounding configuration applies. This does not revoke the current account session.

Configuration commands distinguish local CLI settings from synchronized repository content. Inspect current state before changing it; synchronization reports conflicts instead of silently overwriting one side. The --json option provides structured output for scripts where supported.

JSON output is supported with --json.

```
pb config unset server [flags]
```

### Options

```
  -h, --help   help for unset
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

