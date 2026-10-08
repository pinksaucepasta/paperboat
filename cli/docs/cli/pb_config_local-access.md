## pb config local-access

Configure local browser domains, aliases and reverse proxies

### Synopsis

Configure the browser domain and named service URLs used by this local Paperboat client. These settings only select names for machine ports already authorized elsewhere; they do not grant access or publish additional ports.

With --json, this command group lists its available commands.

### Options

```
  -h, --help   help for local-access
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

* [pb config](pb_config.md)	 - Inspect the local CLI config
* [pb config local-access alias](pb_config_local-access_alias.md)	 - Manage named browser aliases for authorized machine ports
* [pb config local-access apply](pb_config_local-access_apply.md)	 - Apply local browser settings from the config file
* [pb config local-access domain](pb_config_local-access_domain.md)	 - Set the local browser domain
* [pb config local-access proxy](pb_config_local-access_proxy.md)	 - Route machine app names through Coolify or another reverse proxy
* [pb config local-access show](pb_config_local-access_show.md)	 - Show local browser domains and aliases

