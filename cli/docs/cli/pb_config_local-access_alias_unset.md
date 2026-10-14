## pb config local-access alias unset

Remove a service alias

### Synopsis

Remove one named browser mapping for the selected machine. Paperboat withdraws the local name while preserving its machine authorization and other service aliases, then confirms only after applying the updated configuration.

JSON output is supported with --json.

```
pb config local-access alias unset <machine-alias> <name> [flags]
```

### Options

```
  -h, --help   help for unset
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

* [pb config local-access alias](pb_config_local-access_alias.md)	 - Manage named browser aliases for authorized machine ports

