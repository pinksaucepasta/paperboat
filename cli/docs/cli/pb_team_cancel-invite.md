## pb team cancel-invite

Cancel an outstanding invitation

### Synopsis

Cancel an invitation that has not been accepted yet, so it can no longer be used. Team
administrators can cancel invitations of their team; find the ID with pb team invitations <team>.
Pass the team's current generation with --generation (read it with pb team get). A stale
generation is rejected, so two people editing the team never overwrite each other.

JSON output is supported with --json.

```
pb team cancel-invite <team> <invitation> [flags]
```

### Examples

```
  pb team invitations acme
  pb team cancel-invite acme inv_abc123 --generation 8
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

