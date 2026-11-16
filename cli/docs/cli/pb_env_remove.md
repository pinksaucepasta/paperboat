## pb env remove

Remove this machine's local ENV vault custody

### Synopsis

Remove this machine's local ENV vault custody

Remove this machine's local vault custody after exact confirmation. This differs from reset: account ENV values and other authorized machines are not deleted.

ENV values are encrypted before they leave the client. The interactive picker separates shared Team globals, your private member globals, and device overrides in the active workspace; commands select exact scopes explicitly; list output shows metadata rather than secret values. Unlock the local vault when a key operation requires it.

JSON output is supported with --json.

```
pb env remove [flags]
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
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb env](pb_env.md)	 - Manage automatic global ENV and device overrides

