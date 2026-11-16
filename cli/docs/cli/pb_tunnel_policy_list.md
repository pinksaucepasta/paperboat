## pb tunnel policy list

List on-demand port policies (their IDs and generations)

### Synopsis

List on-demand port policies (their IDs and generations)

List this account's on-demand port policies with their IDs, targets, generations and expiry, expired ones included. Use the ID and generation to update or revoke a policy whose ID was lost.

An on-demand policy authorizes an exact machine port for private or team access. A policy does not publish a public URL or grant every port on the machine. Revocation fences subsequent activation.

JSON output is supported with --json.

```
pb tunnel policy list [flags]
```

### Options

```
  -h, --help   help for list
      --json   print canonical JSON
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

