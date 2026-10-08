## pb config status

Show configuration synchronization status

### Synopsis

Show configuration synchronization status

Report synchronization state and any pending review or conflict for one environment or all assignments. Use this before approving or forcing a direction.

Configuration commands distinguish local CLI settings from synchronized repository content. Inspect current state before changing it; synchronization reports conflicts instead of silently overwriting one side. The --json option provides structured output for scripts where supported.

JSON output is supported with --json.

```
pb config status [environment] [flags]
```

### Options

```
  -h, --help   help for status
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

* [pb config](pb_config.md)	 - Inspect the local CLI config

