## pb config team-default-unadopt

Stop inheriting a team configuration default

### Synopsis

Stop inheriting a team configuration default

Stop this account inheriting its adopted team repository default. Personal machine assignments, the team's default, and other members' choices remain unchanged.

Configuration commands distinguish local CLI settings from synchronized repository content. Inspect current state before changing it; synchronization reports conflicts instead of silently overwriting one side. The --json option provides structured output for scripts where supported.

JSON output is supported with --json.

```
pb config team-default-unadopt [flags]
```

### Options

```
  -h, --help   help for team-default-unadopt
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

