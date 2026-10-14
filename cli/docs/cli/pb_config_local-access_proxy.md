## pb config local-access proxy

Route machine app names through Coolify or another reverse proxy

### Synopsis

Otherwise-unmapped app names automatically use authorized port 80. Configure a different reverse-proxy port for a machine. Numeric port URLs and explicit aliases keep their destinations. This local setting never grants access; the proxy port must already be authorized.

With --json, this command group lists its available commands.

### Options

```
  -h, --help   help for proxy
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
* [pb config local-access proxy set](pb_config_local-access_proxy_set.md)	 - Set the machine reverse-proxy port
* [pb config local-access proxy unset](pb_config_local-access_proxy_unset.md)	 - Restore the default reverse-proxy port

