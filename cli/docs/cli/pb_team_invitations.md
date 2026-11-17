## pb team invitations

Discover pending invitations received by you or administered for a team

### Synopsis

List pending invitations: the ones addressed to you, or with a team argument the ones
that team has outstanding (owners and admins). Each line shows invitation ID, team, recipient,
state and expiry. Filter with --q (email or ID); the list stops at --limit unless you pass --all.

JSON output is supported with --json.

```
pb team invitations [team] [flags]
```

### Examples

```
  pb team invitations
  pb team invitations acme --q ana
  pb team invitations --all --json
```

### Options

```
      --all         show every item, up to 10000
  -h, --help        help for invitations
      --json        print canonical JSON
      --limit int   show at most this many items (1-200) (default 50)
      --q string    filter by email or ID
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

