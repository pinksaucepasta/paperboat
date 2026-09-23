## pb env set

Set one environment variable through a hidden prompt or bounded stdin

### Synopsis

Set one environment variable through a hidden prompt or bounded stdin

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

