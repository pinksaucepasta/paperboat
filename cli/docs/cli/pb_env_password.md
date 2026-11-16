## pb env password

Rewrap unlocked ENV keys with a new password

### Synopsis

Rewrap unlocked ENV keys with a new password

Rewrap unlocked vault keys under a new password without changing ENV values. Supply the new password through stdin or a private file rather than a shell argument.

ENV values are encrypted before they leave the client. The interactive picker separates shared Team globals, your private member globals, and device overrides in the active workspace; commands select exact scopes explicitly; list output shows metadata rather than secret values. Unlock the local vault when a key operation requires it.

JSON output is supported with --json.

```
pb env password [flags]
```

### Options

```
  -h, --help                   help for password
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

