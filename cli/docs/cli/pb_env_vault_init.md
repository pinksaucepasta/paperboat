## pb env vault init

Create a password-protected ENV vault

### Synopsis

Create a password-protected ENV vault

Create password-protected local key custody for this account. Read the password from a private file or stdin; --recovery-file saves a newly generated recovery code to a new file. Already registered owned hosts receive an empty encrypted delivery without authorizing new names.

The local vault protects ENV key custody with a password and optional recovery code. Locking clears unlocked keys from this machine; removing local custody differs from resetting and deleting account values. Never place a password or recovery code in shell arguments.

JSON output is supported with --json.

```
pb env vault init [flags]
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

* [pb env vault](pb_env_vault.md)	 - Manage password-protected ENV key custody

