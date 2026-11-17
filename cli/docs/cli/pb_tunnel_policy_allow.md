## pb tunnel policy allow

Allow on-demand access to an exact machine port

### Synopsis

Authorize on-demand access to one exact port of a machine: a bare port number or a loopback
http, https or h2c URL. --access chooses who may use it, private (the default) or team. The
policy lasts --expires (default 8h, must be positive). To change an existing policy pass its
--id together with the --generation you last saw. A replacement application on the same port
inherits the access.

JSON output is supported with --json.

```
pb tunnel policy allow <machine> <port|url> [flags]
```

### Examples

```
  pb tunnel policy allow homelab 3000
  pb tunnel policy allow homelab https://127.0.0.1:8443 --access team --expires 1h
  pb tunnel policy allow homelab 3000 --id pol_abc --generation 3
```

### Options

```
      --access string      access audience: private or team (default "private")
      --expires duration   policy lifetime (default 8h0m0s)
      --generation int     expected existing policy generation
  -h, --help               help for allow
      --id string          existing policy ID to update
      --json               print canonical JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --ephemeral          use the temporary preview lifecycle
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb tunnel policy](pb_tunnel_policy.md)	 - Manage permission to activate a private port on demand

