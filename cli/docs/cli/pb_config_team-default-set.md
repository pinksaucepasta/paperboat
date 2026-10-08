## pb config team-default-set

Set a team's default pull repository

### Synopsis

Set a team's default pull repository

Set a team's default pull repository for members who explicitly adopt it. This changes the team default, not every member's current local assignment.

Configuration commands distinguish local CLI settings from synchronized repository content. Inspect current state before changing it; synchronization reports conflicts instead of silently overwriting one side. The --json option provides structured output for scripts where supported.

JSON output is supported with --json.

```
pb config team-default-set <team> <repository> [flags]
```

### Options

```
  -h, --help   help for team-default-set
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

