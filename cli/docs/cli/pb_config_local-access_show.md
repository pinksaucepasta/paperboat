## pb config local-access show

Show local browser domains and aliases

### Synopsis

Show the effective browser domain and named service mappings stored in the local configuration. This is a read-only view and does not change trust, machine permissions, active routes, or running services.

JSON output is supported with --json.

```
pb config local-access show [flags]
```

### Options

```
  -h, --help   help for show
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

* [pb config local-access](pb_config_local-access.md)	 - Configure local browser domains, aliases and reverse proxies

