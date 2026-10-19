## pb team machine requests

List machines waiting for a team owner or admin

### Synopsis

List machines waiting for a team owner or admin

List machines waiting for a team owner or admin, with who asked and when. Owners and admins see every request; other members see their own, including declined ones.

A personal machine can be shared with a team while ownership stays personal, or explicitly transferred to team ownership. Sharing and capability grants are separate decisions. Removing a team-owned machine revokes its enrollment; unsharing a personal machine withdraws that team's access.

JSON output is supported with --json.

```
pb team machine requests <team> [flags]
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

