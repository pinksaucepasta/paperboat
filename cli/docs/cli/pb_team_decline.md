## pb team decline

Decline an invitation bound to this account

### Synopsis

Decline an invitation bound to this account

Decline a pending invitation explicitly addressed to this account without joining the team. The invitation is cancelled; completed or changed invitations require reloading their current status. Invitations for other accounts cannot be declined.

Teams use explicit membership and resource grants. Membership alone does not grant machine use, ENV values, previews, or tunnels. Mutations use the current team generation to reject stale edits; read pb team get before retrying a conflicting change.

JSON output is supported with --json.

```
pb team decline <invitation> [flags]
```

### Options

```
  -h, --help   help for decline
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

