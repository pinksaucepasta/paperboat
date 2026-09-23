## pb env vault reset

Replace personal ENV keys and delete every personal value

### Synopsis

Replace personal ENV keys and delete every personal value

JSON output is supported with --json.

```
pb env vault reset [flags]
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
```

### SEE ALSO

* [pb env vault](pb_env_vault.md)	 - Manage password-protected ENV key custody

