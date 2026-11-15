## pb config local-access show

Show the local browser domain

### Synopsis

Show the browser domain this computer uses for private browser addresses. This is a read-only view: it does not change trust, machine permissions, active routes, port names or running services.

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

* [pb config local-access](pb_config_local-access.md)	 - Configure the local browser domain

