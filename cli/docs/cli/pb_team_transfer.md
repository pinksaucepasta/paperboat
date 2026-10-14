## pb team transfer

Transfer team ownership

### Synopsis

Transfer team ownership

Transfer team ownership to the named account at the expected generation. Review the target account and current team state before changing owner authority.

Teams use explicit membership and resource grants. Membership alone does not grant machine use, ENV values, previews, or tunnels. Mutations use the current team generation to reject stale edits; read pb team get before retrying a conflicting change.

JSON output is supported with --json.

```
pb team transfer <team> <account> [flags]
```

### Options

```
      --generation uint   expected current team generation
  -h, --help              help for transfer
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

* [pb team](pb_team.md)	 - Manage teams and explicit resource permissions

