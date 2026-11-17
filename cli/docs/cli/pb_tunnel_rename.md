## pb tunnel rename

Rename a durable tunnel

### Synopsis

Rename a durable tunnel, selected by ID or current name. The name is only a label: the
tunnel's address, routes, connectors and access rules do not change.

A change returns once the request is accepted, and the tunnel may still be connecting. Pass
--wait to wait (at most --timeout, default 2m, 1s to 10m) until it finishes, or check pb tunnel
status.

JSON output is supported with --json.

```
pb tunnel rename <tunnel> <name> [flags]
```

### Examples

```
  pb tunnel rename web website
  pb tunnel rename tunnel_abc123 web --wait
```

### Options

```
  -h, --help               help for rename
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

