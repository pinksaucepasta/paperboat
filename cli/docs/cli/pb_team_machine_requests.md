## pb team machine requests

List machines waiting for a team owner or admin

### Synopsis

List machines waiting for an owner or admin to accept them, one line each with machine ID,
name, state and who asked. Prints "No machines are waiting." when there are none.

JSON output is supported with --json.

```
pb team machine requests <team> [flags]
```

### Examples

```
  pb team machine requests acme
  pb team machine requests acme --json
```

### Options

```
  -h, --help   help for requests
      --json   print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb team machine](pb_team_machine.md)	 - Share machines and manage exact team access

