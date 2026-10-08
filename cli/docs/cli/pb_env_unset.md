## pb env unset

Remove one environment variable

### Synopsis

Remove one environment variable

Delete one ENV name from the selected personal, team, or host scope. The confirmation code confirms removal. Previously selected recipients receive a refreshed encrypted delivery with the deleted value omitted; running processes retain their environment.

ENV values are encrypted before they leave the client. Commands select personal, team, or host scope explicitly; list output shows metadata rather than secret values. Unlock the local vault when a key operation requires it.

JSON output is supported with --json.

```
pb env unset <name> [flags]
```

### Options

```
      --confirm string   six-character confirmation code from the preview
  -h, --help             help for unset
      --json             print redacted JSON metadata
      --machine string   Personal override for an owned machine, even in a Team workspace; omitted uses the active workspace
      --team string      team scope; cannot be combined with --machine
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb env](pb_env.md)	 - Manage ENV Injection for connected hosts

