## pb team invitation

Show an invitation you may receive or administer

### Synopsis

Show one invitation you received or administer, as JSON. The invitation ID comes from pb team
invitations. This only displays it; use accept, decline or cancel-invite to act on it.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb team invitation <invitation> [flags]
```

### Examples

```
  pb team invitation inv_abc123
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

