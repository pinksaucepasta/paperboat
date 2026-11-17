## pb team get

Show one team and its current generation

### Synopsis

Show one team: its owner, current generation, state (active, suspended or deleted), the
members and resource grants, the machines shared with or owned by the team, and the team's ENV
status. Every change to the team needs the generation printed here.

JSON output is supported with --json.

```
pb team get <team> [flags]
```

### Examples

```
  pb team get acme
  pb team get acme --json
```

### Options

```
  -h, --help   help for get
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

