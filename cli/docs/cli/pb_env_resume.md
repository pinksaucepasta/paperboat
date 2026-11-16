## pb env resume

Reconcile an interrupted vault publication

### Synopsis

Reconcile an interrupted vault publication

Reconcile a vault publication interrupted after a partial remote transition. Run this before retrying another key operation when the vault reports pending state. Staged recipient delivery retries keep the exact operation and ciphertext, then authorized workspace layers refresh.

ENV values are encrypted before they leave the client. The interactive picker separates shared Team globals, your private member globals, and device overrides in the active workspace; commands select exact scopes explicitly; list output shows metadata rather than secret values. Unlock the local vault when a key operation requires it.

JSON output is supported with --json.

```
pb env resume [flags]
```

### Options

```
  -h, --help   help for resume
      --json   print canonical JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb env](pb_env.md)	 - Manage automatic global ENV and device overrides

