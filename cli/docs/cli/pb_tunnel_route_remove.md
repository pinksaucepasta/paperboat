## pb tunnel route remove

Remove a tunnel route

### Synopsis

Remove a tunnel route

Remove one route from a durable tunnel after confirmation. The tunnel and its other routes and connectors remain available.

Routes map a public or private match to a specific origin and protocol. TLS and host-header options affect the trust boundary and should match the origin. Adding or updating a route may return an operation whose readiness must be checked separately.

JSON output is supported with --json.

```
pb tunnel route remove <tunnel> <route> [flags]
```

### Options

```
      --confirm string     six-character confirmation code from the preview
  -h, --help               help for remove
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

* [pb tunnel route](pb_tunnel_route.md)	 - Manage tunnel routes

