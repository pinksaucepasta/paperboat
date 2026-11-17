## pb team delete

Delete a team

### Synopsis

Delete a team. Team-owned machines are revoked; personal resources stay with their owners.
Check pb team get first for machines and attached resources.

--confirm must repeat the exact team ID, so a typo cannot delete the wrong team.
Pass the team's current generation with --generation (read it with pb team get). A stale
generation is rejected, so two people editing the team never overwrite each other.

JSON output is supported with --json.

```
pb team delete <team> [flags]
```

### Examples

```
  pb team delete acme --confirm acme --generation 9
```

### Options

```
      --confirm string    exact team identifier
      --generation uint   expected current team generation
  -h, --help              help for delete
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

