## pb team machine approval

Set whether machines members add wait for an owner or admin

### Synopsis

Set whether machines members add wait for an owner or admin

Choose whether machines that members add to the team wait for an owner or admin (on, the default) or join right away (off). Owners and admins add machines directly either way. Pending requests stay pending when the rule is switched off.

A personal machine can be shared with a team while ownership stays personal, or explicitly transferred to team ownership. Sharing and capability grants are separate decisions. Removing a team-owned machine revokes its enrollment; unsharing a personal machine withdraws that team's access.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb team machine approval <team> <on|off> [flags]
```

### Options

```
  -h, --help   help for approval
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb team machine](pb_team_machine.md)	 - Share machines and manage exact team access

