## pb env vault remove

Remove this device's local ENV vault custody

### Synopsis

Remove this device's local ENV vault custody

Remove this device's local vault custody after exact confirmation. This differs from reset: account ENV values and other authorized devices are not deleted.

The local vault protects ENV key custody with a password and optional recovery code. Locking clears unlocked keys from this device; removing local custody differs from resetting and deleting account values. Never place a password or recovery code in shell arguments.

JSON output is supported with --json.

```
pb env vault remove [flags]
```

### Options

```
      --confirm string   exact confirmation phrase: REMOVE LOCAL ENV <account_id>
  -h, --help             help for remove
      --json             print canonical JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb env vault](pb_env_vault.md)	 - Manage password-protected ENV key custody

