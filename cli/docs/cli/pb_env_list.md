## pb env list

List configured environment-variable metadata

### Synopsis

List configured environment-variable metadata

List ENV names and scope metadata, optionally limited to a team or host. This does not print plaintext values or key material.

ENV values are encrypted before they leave the client. The interactive picker separates shared Team globals, your private member globals, and device overrides in the active workspace; commands select exact scopes explicitly; list output shows metadata rather than secret values. Unlock the local vault when a key operation requires it.

JSON output is supported with --json.

```
pb env list [flags]
```

### Options

```
  -h, --help             help for list
      --json             print redacted JSON metadata
      --machine string   Private device override in the active workspace; omitted uses your global values
      --team string      team scope; defaults to this account's personal scope
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

