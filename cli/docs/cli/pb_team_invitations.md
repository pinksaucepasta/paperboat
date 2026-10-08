## pb team invitations

Discover pending invitations received by you or administered for a team

### Synopsis

Discover pending invitations received by you or administered for a team

Discover every authorized invitation for your account, or list invitations for a team you administer. Inventory pagination is followed to completion. Use the exact invitation identity to inspect, accept a received invitation, or cancel an invitation you administer.

Teams use explicit membership and resource grants. Membership alone does not grant machine use, ENV values, previews, or tunnels. Mutations use the current team generation to reject stale edits; read pb team get before retrying a conflicting change.

JSON output is supported with --json.

```
pb team invitations [team] [flags]
```

### Options

```
  -h, --help   help for invitations
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

