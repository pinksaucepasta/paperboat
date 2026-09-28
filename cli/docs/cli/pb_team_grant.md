## pb team grant

Set or revoke an explicit resource permission

### Synopsis

Set or revoke an explicit resource permission

Set or revoke an explicit ENV, preview, or tunnel permission for one account and resource. --active selects grant state, and --generation rejects stale team edits.

Teams use explicit membership and resource grants. Membership alone does not grant device use, ENV values, previews, or tunnels. Mutations use the current team generation to reject stale edits; read pb team get before retrying a conflicting change.

JSON output is supported with --json.

```
pb team grant <team> <account> <env|preview|tunnel> <resource> <permission> [flags]
```

### Options

```
      --active            grant permission; set false to revoke (default true)
      --generation uint   expected current team generation
  -h, --help              help for grant
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

