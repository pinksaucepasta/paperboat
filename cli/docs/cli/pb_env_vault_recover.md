## pb env vault recover

Recover vault access and replace the password and recovery code

### Synopsis

Recover vault access and replace the password and recovery code

JSON output is supported with --json.

```
pb env vault recover [flags]
```

### Options

```
  -h, --help                         help for recover
      --json                         print canonical JSON
      --password-file string         read the raw master password from an absolute owner-only file
      --password-stdin               read the raw master password from non-interactive stdin
      --recovery-file string         save a newly generated recovery code to a new absolute file path
      --recovery-input-file string   read the old recovery code from an absolute owner-only file
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb env vault](pb_env_vault.md)	 - Manage password-protected ENV key custody

