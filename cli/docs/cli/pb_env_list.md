## pb env list

List configured environment-variable metadata

### Synopsis

List configured environment-variable metadata

JSON output is supported with --json.

```
pb env list [flags]
```

### Options

```
  -h, --help             help for list
      --json             print redacted JSON metadata
      --machine string   machine name or ID; defaults to the personal scope
      --team string      team scope; defaults to this account's personal scope
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb env](pb_env.md)	 - Manage ENV Injection for connected hosts

