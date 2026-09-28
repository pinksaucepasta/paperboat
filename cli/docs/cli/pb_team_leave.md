## pb team leave

Leave a team

### Synopsis

Leave a team

Leave your current membership in one team at the expected generation. Access supplied by that membership is withdrawn; owned resources require separate handling.

Teams use explicit membership and resource grants. Membership alone does not grant device use, ENV values, previews, or tunnels. Mutations use the current team generation to reject stale edits; read pb team get before retrying a conflicting change.

JSON output is supported with --json.

```
pb team leave <team> [flags]
```

### Options

```
      --generation uint   expected current team generation
  -h, --help              help for leave
      --json              print canonical JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb team](pb_team.md)	 - Manage teams and explicit resource permissions

