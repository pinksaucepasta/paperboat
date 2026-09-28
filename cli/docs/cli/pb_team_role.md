## pb team role

Change a team member's role

### Synopsis

Change a team member's role

Set one member's role to admin or member at the expected generation. This changes team management authority, not the member's explicit resource grants.

Teams use explicit membership and resource grants. Membership alone does not grant device use, ENV values, previews, or tunnels. Mutations use the current team generation to reject stale edits; read pb team get before retrying a conflicting change.

JSON output is supported with --json.

```
pb team role <team> <account> <admin|member> [flags]
```

### Options

```
      --generation uint   expected current team generation
  -h, --help              help for role
      --json              print canonical JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb team](pb_team.md)	 - Manage teams and explicit resource permissions

