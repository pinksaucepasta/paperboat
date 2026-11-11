## pb team machine transfer-to-personal

Return a team-owned machine to the personal account that enrolled it

### Synopsis

Return a team-owned machine to the personal account that enrolled it

Return a team-owned machine to the personal account that enrolled it, after exact confirmation and generation check. Needs a team owner or admin; the enroller must still be in the team. The machine keeps its id, keys and history and is not re-enrolled; the team's grants and shares for it end, and previews and tunnels the team owns on it must be ended first. Accounts other than the enroller cannot take it: its credentials belong to the enroller.

A personal machine can be shared with a team while ownership stays personal, or explicitly transferred to team ownership and back to the personal account that enrolled it. Sharing and capability grants are separate decisions. Removing a team-owned machine revokes its enrollment; unsharing a personal machine withdraws that team's access.

JSON output is supported with --json.

```
pb team machine transfer-to-personal <team> <machine-id> [flags]
```

### Options

```
      --confirm string    exact machine identifier acknowledging the ownership or revocation effect
      --generation uint   expected current team generation
  -h, --help              help for transfer-to-personal
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

