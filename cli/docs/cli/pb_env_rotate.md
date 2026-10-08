## pb env rotate

Rotate personal ENV scope keys while preserving values

### Synopsis

Rotate personal ENV scope keys while preserving values

Rotate personal ENV scope keys while retaining the values. Complete or cancel an interrupted rotation before starting another custody change.

ENV values are encrypted before they leave the client. The interactive picker separates shared Team globals, your private member globals, and device overrides in the active workspace; commands select exact scopes explicitly; list output shows metadata rather than secret values. Unlock the local vault when a key operation requires it.

JSON output is supported with --json.

```
pb env rotate [flags]
```

### Options

```
  -h, --help   help for rotate
      --json   print JSON
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
* [pb env rotate cancel](pb_env_rotate_cancel.md)	 - Cancel an uncommitted personal ENV key rotation

