## pb config unassign

Remove a config repository assignment

### Synopsis

Remove a config repository assignment

Remove a repository assignment from the selected environment. The repository and its content remain; the confirmation code confirms the local synchronization change.

Configuration commands distinguish local CLI settings from synchronized repository content. Inspect current state before changing it; synchronization reports conflicts instead of silently overwriting one side. The --json option provides structured output for scripts where supported.

JSON output is supported with --json.

```
pb config unassign <environment> [flags]
```

### Options

```
      --confirm string   six-character confirmation code from the preview
  -h, --help             help for unassign
      --json             print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb config](pb_config.md)	 - Inspect the local CLI config

