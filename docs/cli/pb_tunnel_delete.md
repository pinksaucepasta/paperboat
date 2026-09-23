## pb tunnel delete

Delete endpoints and routes and revoke connectors while preserving user DNS records

### Synopsis

Delete endpoints and routes and revoke connectors while preserving user DNS records

JSON output is supported with --json.

```
pb tunnel delete <tunnel> [flags]
```

### Options

```
  -h, --help               help for delete
      --json               print canonical JSON
      --timeout duration   maximum time to wait for operation completion (default 2m0s)
      --wait               wait for the operation to reach a terminal state
      --yes                confirm endpoint, route, domain-binding, and connector-credential removal
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --ephemeral          use the temporary preview lifecycle
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb tunnel](pb_tunnel.md)	 - Manage durable tunnels or start an ephemeral tunnel

