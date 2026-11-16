## pb env reset

Replace personal ENV keys and delete every personal value

### Synopsis

Replace personal ENV keys and delete every personal value

Replace personal ENV keys and delete every personal value after exact account confirmation. This is destructive and differs from local lock, remove, or password change.

ENV values are encrypted before they leave the client. The interactive picker separates shared Team globals, your private member globals, and device overrides in the active workspace; commands select exact scopes explicitly; list output shows metadata rather than secret values. Unlock the local vault when a key operation requires it.

JSON output is supported with --json.

```
pb env reset [flags]
```

### Options

```
      --confirm string         exact confirmation phrase: RESET ENV <account_id>
  -h, --help                   help for reset
      --json                   print canonical JSON
      --password-file string   read the raw master password from an absolute owner-only file
      --password-stdin         read the raw master password from non-interactive stdin
      --recovery-file string   save a newly generated recovery code to a new absolute file path
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

