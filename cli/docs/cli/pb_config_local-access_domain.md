## pb config local-access domain

Set the local browser domain

### Synopsis

Choose the DNS domain used for this user's local browser URLs. Changing the domain provisions local trust through the normal privileged installation path and updates the daemon before this command reports success.

With --json, this command group lists its available commands.

### Options

```
  -h, --help   help for domain
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
* [pb config local-access domain reset](pb_config_local-access_domain_reset.md)	 - Restore the default local browser domain
* [pb config local-access domain set](pb_config_local-access_domain_set.md)	 - Set and apply the local browser domain

