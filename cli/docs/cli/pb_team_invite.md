## pb team invite

Invite an account as a member

### Synopsis

Invite an account as a member

Invite one exact account to the selected team at its current generation. The recipient must accept; an invitation is not immediate membership or a resource grant.

Teams use explicit membership and resource grants. Membership alone does not grant device use, ENV values, previews, or tunnels. Mutations use the current team generation to reject stale edits; read pb team get before retrying a conflicting change.

JSON output is supported with --json.

```
pb team invite <team> <account> [flags]
```

### Options

```
      --generation uint   expected current team generation
  -h, --help              help for invite
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

