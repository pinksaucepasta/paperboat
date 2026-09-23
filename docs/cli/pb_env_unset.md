## pb env unset

Remove one environment variable

### Synopsis

Remove one environment variable

JSON output is supported with --json.

```
pb env unset <name> [flags]
```

### Options

```
  -h, --help             help for unset
      --json             print redacted JSON metadata
      --machine string   machine name or ID; defaults to the personal scope
      --team string      team scope; cannot be combined with --machine
      --yes              confirm removal
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb env](pb_env.md)	 - Manage ENV Injection for connected hosts

