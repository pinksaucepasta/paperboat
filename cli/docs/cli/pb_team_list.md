## pb team list

List teams

### Synopsis

List teams

List teams visible to the signed-in account and their basic state. Select one with pb team get to inspect its current generation and grants.

Teams use explicit membership and resource grants. Membership alone does not grant machine use, ENV values, previews, or tunnels. Mutations use the current team generation to reject stale edits; read pb team get before retrying a conflicting change.

JSON output is supported with --json.

```
pb team list [flags]
```

### Options

```
  -h, --help   help for list
      --json   print canonical JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb team](pb_team.md)	 - Manage teams and explicit resource permissions

