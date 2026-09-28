## pb tunnel delete

Delete endpoints and routes and revoke connectors while preserving user DNS records

### Synopsis

Delete endpoints and routes and revoke connectors while preserving user DNS records

Delete one durable tunnel's endpoints and routes and revoke its connectors, while preserving user-managed DNS records. the confirmation code confirms removal; inspect the tunnel and domains first.

Durable tunnels retain identity, routes, connectors, and domains across client sessions. Ephemeral tunnels have a separate lifecycle. Route publication and browser HTTP access require exact authorization; TLS for browser HTTP terminates at the edge.

JSON output is supported with --json.

```
pb tunnel delete <tunnel> [flags]
```

### Options

```
      --confirm string     six-character confirmation code from the preview
  -h, --help               help for delete
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
```

### SEE ALSO

* [pb tunnel](pb_tunnel.md)	 - Manage durable tunnels or start an ephemeral tunnel

