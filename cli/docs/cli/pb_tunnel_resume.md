## pb tunnel resume

Resume new traffic for a preserved tunnel

### Synopsis

Let a paused durable tunnel carry new traffic again, with the same address and routes. If the
machine is offline or the origin is down it will still fail; check pb tunnel status.

A change returns once the request is accepted, and the tunnel may still be connecting. Pass
--wait to wait (at most --timeout, default 2m, 1s to 10m) until it finishes, or check pb tunnel
status.

JSON output is supported with --json.

```
pb tunnel resume <tunnel> [flags]
```

### Examples

```
  pb tunnel resume web --wait
```

### Options

```
  -h, --help               help for resume
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

