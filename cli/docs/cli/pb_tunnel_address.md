## pb tunnel address

Change a durable tunnel's public address

### Synopsis

Change the public address of a durable tunnel selected by its exact ID or name: <subdomain> becomes the first label of its address under the same base domain: helloworld makes helloworld.<base domain>. The subdomain is 3 to 63 lowercase letters, digits or hyphens, not starting or ending with a hyphen and not only digits; some names are reserved, and a name another tunnel uses is refused. The previous address stops working at once and may be taken by someone else. Routes, connectors and access policy stay attached to the tunnel, and links to the old address break, so update anything that uses it. Changes are rate limited per account.

JSON output is supported with --json.

```
pb tunnel address <tunnel> <subdomain> [flags]
```

### Options

```
  -h, --help               help for address
      --json               print canonical JSON
      --timeout duration   maximum time to wait for operation completion (default 2m0s)
      --wait               wait for the operation to reach a terminal state
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

* [pb tunnel](pb_tunnel.md)	 - Manage durable tunnels or start an ephemeral tunnel

