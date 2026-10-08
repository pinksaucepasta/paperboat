## pb config local-access apply

Apply local browser settings from the config file

### Synopsis

Validate and apply local browser settings after editing the configuration file directly. Domain changes use the ordinary privileged trust workflow; aliases remain bounded to already authorized machine ports, and errors leave success unreported.

JSON output is supported with --json.

```
pb config local-access apply [flags]
```

### Options

```
  -h, --help   help for apply
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

