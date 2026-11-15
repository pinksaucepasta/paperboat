## pb config local-access

Configure the local browser domain

### Synopsis

Configure the DNS domain this computer uses for private browser addresses such as https://3000.homelab.<domain>. Port names and a machine's front desk are stored on the server and set with `pb port name set` and `pb port front-desk`.

With --json, this command group lists its available commands.

```
pb config local-access [flags]
```

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
* [pb config local-access domain](pb_config_local-access_domain.md)	 - Set the local browser domain
* [pb config local-access show](pb_config_local-access_show.md)	 - Show the local browser domain

