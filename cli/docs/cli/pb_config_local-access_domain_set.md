## pb config local-access domain set

Set and apply the local browser domain

### Synopsis

Set the browser domain for this local installation. Paperboat validates its DNS scope, provisions the matching local trust, updates the running helper, and persists the setting only through the standard apply workflow.

JSON output is supported with --json.

```
pb config local-access domain set <domain> [flags]
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

* [pb config local-access domain](pb_config_local-access_domain.md)	 - Set the local browser domain

