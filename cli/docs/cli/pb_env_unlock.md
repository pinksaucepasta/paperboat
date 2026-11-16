## pb env unlock

Unlock this account's ENV vault on this machine

### Synopsis

Unlock this account's ENV vault on this machine

Unlock encrypted local custody with the password supplied interactively, by stdin, or from a private file. Authorized workspace layers refresh automatically after unlock. On headless Linux without a persistent secure store, keys remain in the user daemon memory for 15 minutes; lock or daemon restart clears them. Locked keys never publish a delivery. No ENV value is printed by unlocking.

ENV values are encrypted before they leave the client. The interactive picker separates shared Team globals, your private member globals, and device overrides in the active workspace; commands select exact scopes explicitly; list output shows metadata rather than secret values. Unlock the local vault when a key operation requires it.

JSON output is supported with --json.

```
pb env unlock [flags]
```

### Options

```
  -h, --help                   help for unlock
      --json                   print canonical JSON
      --password-file string   read the raw master password from an absolute owner-only file
      --password-stdin         read the raw master password from non-interactive stdin
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

