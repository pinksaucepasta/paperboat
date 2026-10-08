## pb env vault lock

Clear unlocked vault keys while retaining encrypted custody

### Synopsis

Clear unlocked vault keys while retaining encrypted custody

Clear unlocked ENV keys from this machine while retaining encrypted custody. Unlock with the password before another key-dependent operation.

The local vault protects ENV key custody with a password and optional recovery code. Locking clears unlocked keys from this machine; removing local custody differs from resetting and deleting account values. Never place a password or recovery code in shell arguments.

JSON output is supported with --json.

```
pb env vault lock [flags]
```

### Options

```
  -h, --help   help for lock
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

* [pb env vault](pb_env_vault.md)	 - Manage password-protected ENV key custody

