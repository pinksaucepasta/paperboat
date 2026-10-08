## pb team invitation

Show an invitation you may receive or administer

### Synopsis

Show an invitation you may receive or administer

Read one exact invitation visible to your account or team administration authority. This command displays metadata and does not accept or cancel it. Use the corresponding explicit invitation action after reviewing its team and recipient.

Teams use explicit membership and resource grants. Membership alone does not grant machine use, ENV values, previews, or tunnels. Mutations use the current team generation to reject stale edits; read pb team get before retrying a conflicting change.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb team invitation <invitation> [flags]
```

### Options

```
  -h, --help   help for invitation
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb team](pb_team.md)	 - Manage teams and explicit resource permissions

