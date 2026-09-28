## pb env vault unlock

Unlock this account's ENV vault on this device

### Synopsis

Unlock this account's ENV vault on this device

Unlock encrypted local custody with the password supplied interactively, by stdin, or from a private file. No ENV value is printed by unlocking.

The local vault protects ENV key custody with a password and optional recovery code. Locking clears unlocked keys from this device; removing local custody differs from resetting and deleting account values. Never place a password or recovery code in shell arguments.

JSON output is supported with --json.

```
pb env vault unlock [flags]
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
```

### SEE ALSO

* [pb env vault](pb_env_vault.md)	 - Manage password-protected ENV key custody

