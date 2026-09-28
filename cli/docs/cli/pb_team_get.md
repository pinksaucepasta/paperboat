## pb team get

Show one team and its current generation

### Synopsis

Show one team and its current generation

Read one team's current membership, permissions, and generation. Use the returned generation for the next mutation to avoid overwriting a newer change.

Teams use explicit membership and resource grants. Membership alone does not grant device use, ENV values, previews, or tunnels. Mutations use the current team generation to reject stale edits; read pb team get before retrying a conflicting change.

JSON output is supported with --json.

```
pb team get <team> [flags]
```

### Options

```
  -h, --help   help for get
      --json   print canonical JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb team](pb_team.md)	 - Manage teams and explicit resource permissions

