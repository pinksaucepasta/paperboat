## pb config local-access alias

Manage named browser aliases for authorized machine ports

### Synopsis

Manage optional DNS names for ports that are already authorized on a machine. Alias configuration never creates a port grant; the browser gateway checks the existing authorization when it resolves and serves each request.

With --json, this command group lists its available commands.

### Options

```
  -h, --help   help for alias
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb config local-access](pb_config_local-access.md)	 - Configure local browser domains, aliases and reverse proxies
* [pb config local-access alias set](pb_config_local-access_alias_set.md)	 - Set and apply a service alias
* [pb config local-access alias unset](pb_config_local-access_alias_unset.md)	 - Remove a service alias

