## pb config force

Force a scoped configuration direction

### Synopsis

Force a scoped configuration direction

Force one pull or push direction for the named environment, optionally limited to a path. This bypasses the normal conflict choice for that scope; the confirmation code confirms the direction.

Configuration commands distinguish local CLI settings from synchronized repository content. Inspect current state before changing it; synchronization reports conflicts instead of silently overwriting one side. The --json option provides structured output for scripts where supported.

JSON output is supported with --json.

```
pb config force <pull|push> <environment> [path] [flags]
```

### Options

```
      --confirm string   six-character confirmation code from the preview
  -h, --help             help for force
      --json             print JSON
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

