## pb env host

Inspect automatic workspace ENV delivery

### Synopsis

Inspect automatic workspace ENV delivery

Inspect automatic encrypted ENV delivery for the active workspace. Personal globals apply to your devices with device overrides; Team globals have private member and device overrides. Delivery does not require selecting names or provisioning a host.

ENV values are encrypted before they leave the client. The interactive picker separates shared Team globals, your private member globals, and device overrides in the active workspace; commands select exact scopes explicitly; list output shows metadata rather than secret values. Unlock the local vault when a key operation requires it.

With --json, this command group lists its available commands.

### Options

```
  -h, --help   help for host
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

* [pb env](pb_env.md)	 - Manage automatic global ENV and device overrides
* [pb env host show](pb_env_host_show.md)	 - Show published ENV delivery and observed application on a host

