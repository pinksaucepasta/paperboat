## pb env init

Create a password-protected ENV vault

### Synopsis

Create a password-protected ENV vault

Create password-protected local key custody for this account. Read the password from a private file or stdin; --recovery-file saves a newly generated recovery code to a new file. Already registered owned hosts receive an empty encrypted delivery without authorizing new names.

ENV values are encrypted before they leave the client. The interactive picker separates shared Team globals, your private member globals, and device overrides in the active workspace; commands select exact scopes explicitly; list output shows metadata rather than secret values. Unlock the local vault when a key operation requires it.

JSON output is supported with --json.

```
pb env init [flags]
```

### Options

```
  -h, --help                   help for init
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

