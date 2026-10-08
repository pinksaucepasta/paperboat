## pb env grants

Reconcile encrypted ENV team grants

### Synopsis

Reconcile encrypted ENV team grants

Reconcile encrypted grants offered by a team into this account's vault. Use sync after a team has explicitly granted access.

ENV values are encrypted before they leave the client. Commands select personal, team, or host scope explicitly; list output shows metadata rather than secret values. Unlock the local vault when a key operation requires it.

With --json, this command group lists its available commands.

### Options

```
  -h, --help   help for grants
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

* [pb env](pb_env.md)	 - Manage ENV Injection for connected hosts
* [pb env grants sync](pb_env_grants_sync.md)	 - Accept pending team grants into this account's encrypted vault

