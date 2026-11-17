## pb team decline

Decline an invitation bound to this account

### Synopsis

Decline an invitation addressed to this account, so you do not join the team. The invitation
ID comes from pb team invitations. Invitations addressed to other accounts cannot be declined.

JSON output is supported with --json.

```
pb team decline <invitation> [flags]
```

### Examples

```
  pb team invitations
  pb team decline inv_abc123
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

