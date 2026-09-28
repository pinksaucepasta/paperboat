## pb team device grant

Set all-member or selected-member device capabilities

### Synopsis

Set the complete capability list for one all-member or selected-member grant. Effective access is the union of both grants. Use --active=false to revoke this grant. Team roles alone do not grant device use.

Set the complete device capability list for one member or all members at the expected team generation. --active can withdraw a grant; omitted capabilities are not implied.

A personal device can be shared with a team while ownership stays personal, or explicitly transferred to team ownership. Sharing and capability grants are separate decisions. Removing a team-owned device revokes its enrollment; unsharing a personal device withdraws that team's access.

JSON output is supported with --json.

```
pb team device grant <team> <device-id> [flags]
```

### Options

```
      --active               set false to revoke this grant (default true)
      --all-members          grant every current and future accepted team member
      --capability strings   exact capabilities: terminal,exec,managed_ssh,files,preview_manage,tunnel_manage
      --generation uint      expected current team generation
  -h, --help                 help for grant
      --json                 print canonical JSON
      --member string        grant only this accepted member account
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb team device](pb_team_device.md)	 - Share devices and manage exact team access

