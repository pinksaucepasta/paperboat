## pb config approve

Approve the currently reviewed pull revision

### Synopsis

Approve the currently reviewed pull revision

Approve the pull revision currently under review for the selected environment. This applies the reviewed revision; it does not approve an unrelated later repository change.

Configuration commands distinguish local CLI settings from synchronized repository content. Inspect current state before changing it; synchronization reports conflicts instead of silently overwriting one side. The --json option provides structured output for scripts where supported.

JSON output is supported with --json.

```
pb config approve <environment> [flags]
```

### Options

```
  -h, --help   help for approve
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

