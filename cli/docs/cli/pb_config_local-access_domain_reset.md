## pb config local-access domain reset

Restore the default local browser domain

### Synopsis

Restore local.pprbt.dev as this client's browser domain and apply the change. The helper and trust state are reconciled before success is printed; configured named services remain attached to their existing machine ports.

JSON output is supported with --json.

```
pb config local-access domain reset [flags]
```

### Options

```
  -h, --help   help for reset
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

