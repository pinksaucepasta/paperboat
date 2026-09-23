## pb env vault password

Rewrap unlocked ENV keys with a new password

### Synopsis

Rewrap unlocked ENV keys with a new password

JSON output is supported with --json.

```
pb env vault password [flags]
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
```

### SEE ALSO

* [pb env vault](pb_env_vault.md)	 - Manage password-protected ENV key custody

