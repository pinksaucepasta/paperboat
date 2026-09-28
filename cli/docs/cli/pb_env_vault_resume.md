## pb env vault resume

Reconcile an interrupted vault publication

### Synopsis

Reconcile an interrupted vault publication

Reconcile a vault publication interrupted after a partial remote transition. Run this before retrying another key operation when the vault reports pending state.

The local vault protects ENV key custody with a password and optional recovery code. Locking clears unlocked keys from this device; removing local custody differs from resetting and deleting account values. Never place a password or recovery code in shell arguments.

JSON output is supported with --json.

```
pb env vault resume [flags]
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
```

### SEE ALSO

* [pb env vault](pb_env_vault.md)	 - Manage password-protected ENV key custody

