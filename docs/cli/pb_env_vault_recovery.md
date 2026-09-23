## pb env vault recovery

Enable, replace, or disable the optional recovery code

### Synopsis

Enable, replace, or disable the optional recovery code

JSON output is supported with --json.

```
pb env vault recovery [flags]
```

### Options

```
      --disable                disable recovery-code access to the current vault
  -h, --help                   help for recovery
      --json                   print canonical JSON
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

