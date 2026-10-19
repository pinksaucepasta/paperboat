## pb team machine approve

Accept a machine a member asked to add to the team

### Synopsis

Accept a machine a member asked to add to the team

Accept a machine a member asked to add to the team (see pb team machine requests). The machine is then shared with the team; access still needs explicit capability grants. Owners and admins only.

A personal machine can be shared with a team while ownership stays personal, or explicitly transferred to team ownership. Sharing and capability grants are separate decisions. Removing a team-owned machine revokes its enrollment; unsharing a personal machine withdraws that team's access.

JSON output is supported with --json.

```
pb team machine approve <team> <machine-id> [flags]
```

### Options

```
      --generation uint   expected current team generation (default: the current one)
  -h, --help              help for approve
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

* [pb team machine](pb_team_machine.md)	 - Share machines and manage exact team access

