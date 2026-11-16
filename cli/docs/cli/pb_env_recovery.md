## pb env recovery

Enable, replace, or disable the optional recovery code

### Synopsis

Enable, replace, or disable the optional recovery code

Enable, replace, or disable optional recovery-code custody. Disabling recovery removes that recovery path without deleting ENV values.

ENV values are encrypted before they leave the client. The interactive picker separates shared Team globals, your private member globals, and device overrides in the active workspace; commands select exact scopes explicitly; list output shows metadata rather than secret values. Unlock the local vault when a key operation requires it.

JSON output is supported with --json.

```
pb env recovery [flags]
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
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb env](pb_env.md)	 - Manage automatic global ENV and device overrides

