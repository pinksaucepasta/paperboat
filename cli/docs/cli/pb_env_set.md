## pb env set

Set one environment variable through a hidden prompt or bounded stdin

### Synopsis

Set one environment variable through a hidden prompt or bounded stdin

Set one exact ENV name using a hidden interactive prompt, a bounded stdin stream, or --value-file. Use --team or --machine to choose a nonpersonal scope; the value is never part of the command output.

ENV values are encrypted before they leave the client. Commands select personal, team, or host scope explicitly; list output shows metadata rather than secret values. Unlock the local vault when a key operation requires it.

JSON output is supported with --json.

```
pb env set <name> [flags]
```

### Options

```
  -h, --help                help for set
      --json                print redacted JSON metadata
      --machine string      machine name or ID; defaults to the personal scope
      --team string         team scope; cannot be combined with --machine
      --value-file string   read the raw value from an absolute file path
      --value-stdin         read the raw value from non-interactive stdin
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb env](pb_env.md)	 - Manage ENV Injection for connected hosts

