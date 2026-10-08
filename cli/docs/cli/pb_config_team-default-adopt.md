## pb config team-default-adopt

Adopt a team default using your provider access

### Synopsis

Adopt a team default using your provider access

Adopt the selected team's default pull repository for this account using its provider access. Adoption is explicit; personal machine assignments still take precedence.

Configuration commands distinguish local CLI settings from synchronized repository content. Inspect current state before changing it; synchronization reports conflicts instead of silently overwriting one side. The --json option provides structured output for scripts where supported.

JSON output is supported with --json.

```
pb config team-default-adopt <team> [flags]
```

### Options

```
  -h, --help   help for team-default-adopt
      --json   
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

