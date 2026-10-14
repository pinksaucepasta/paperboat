## pb team cancel-invite

Cancel an outstanding invitation

### Synopsis

Cancel an outstanding invitation

Cancel one outstanding invitation for the selected team at its expected generation. The invited account cannot accept that invitation afterward.

Teams use explicit membership and resource grants. Membership alone does not grant machine use, ENV values, previews, or tunnels. Mutations use the current team generation to reject stale edits; read pb team get before retrying a conflicting change.

JSON output is supported with --json.

```
pb team cancel-invite <team> <invitation> [flags]
```

### Options

```
      --generation uint   expected current team generation
  -h, --help              help for cancel-invite
      --json              print canonical JSON
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

