## pb team machine grant

Set all-member or selected-member machine capabilities

### Synopsis

Set the complete capability list for one all-member or selected-member grant. Effective access is the union of both grants. Use --active=false to revoke this grant. Team roles alone do not grant machine use.

JSON output is supported with --json.

```
pb team machine grant <team> <machine-id> [flags]
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

* [pb team machine](pb_team_machine.md)	 - Share machines and manage exact team access

