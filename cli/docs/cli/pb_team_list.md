## pb team list

List teams

### Synopsis

List the teams this account belongs to, one line each: team ID, owner account and current
generation, sorted by team ID. Use pb team get for members, grants and machines.

JSON output is supported with --json.

```
pb team list [flags]
```

### Examples

```
  pb team list
  pb team list --json
```

### Options

```
  -h, --help   help for list
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

