## pb env rotate cancel

Cancel an uncommitted personal ENV key rotation

### Synopsis

Cancel an uncommitted personal ENV key rotation

Cancel an uncommitted personal key rotation using the exact confirmation. A completed rotation cannot be undone by this command.

ENV values are encrypted before they leave the client. Commands select personal, team, or host scope explicitly; list output shows metadata rather than secret values. Unlock the local vault when a key operation requires it.

JSON output is supported with --json.

```
pb env rotate cancel [flags]
```

### Options

```
      --confirm string   exact confirmation phrase: CANCEL ENV ROTATE <account_id>
  -h, --help             help for cancel
      --json             print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb env rotate](pb_env_rotate.md)	 - Rotate personal ENV scope keys while preserving values

