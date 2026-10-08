## pb team machine share

Share your personal machine with a team; grants are separate

### Synopsis

Share your personal machine with a team; grants are separate

Share a personal machine with one team while keeping personal ownership. Members need explicit capability grants before they can use it.

A personal machine can be shared with a team while ownership stays personal, or explicitly transferred to team ownership. Sharing and capability grants are separate decisions. Removing a team-owned machine revokes its enrollment; unsharing a personal machine withdraws that team's access.

JSON output is supported with --json.

```
pb team machine share <team> <machine-id> [flags]
```

### Options

```
      --generation uint   expected current team generation
  -h, --help              help for share
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

