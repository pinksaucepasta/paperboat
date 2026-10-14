## pb config local-access alias set

Set and apply a service alias

### Synopsis

Map a nonnumeric DNS service name on one machine alias to an already authorized port. Paperboat validates and applies the local mapping before reporting success, without creating or expanding machine access grants.

JSON output is supported with --json.

```
pb config local-access alias set <machine-alias> <name> <port> [flags]
```

### Options

```
  -h, --help   help for set
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

