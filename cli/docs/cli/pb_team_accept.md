## pb team accept

Accept an invitation bound to this account

### Synopsis

Accept an invitation addressed to this account and join the team. Find the invitation ID
with pb team invitations. The command prints the team as it is after you joined.

JSON output is supported with --json.

```
pb team accept <invitation> [flags]
```

### Examples

```
  pb team invitations
  pb team accept inv_abc123
```

### Options

```
  -h, --help   help for accept
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

