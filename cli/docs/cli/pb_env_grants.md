## pb env grants

Reconcile encrypted ENV team grants

### Synopsis

Reconcile encrypted ENV team grants

Reconcile encrypted grants offered by a team into this account's vault. Current grants are accepted automatically on unlock and refresh; sync provides an explicit recovery action.

ENV values are encrypted before they leave the client. The interactive picker separates shared Team globals, your private member globals, and device overrides in the active workspace; commands select exact scopes explicitly; list output shows metadata rather than secret values. Unlock the local vault when a key operation requires it.

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

* [pb env](pb_env.md)	 - Manage automatic global ENV and device overrides
* [pb env grants sync](pb_env_grants_sync.md)	 - Accept pending team grants into this account's encrypted vault

