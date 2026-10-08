## pb tunnel pause

Pause new traffic while preserving tunnel identity and configuration

### Synopsis

Pause new traffic while preserving tunnel identity and configuration

Pause new traffic for one durable tunnel while retaining its identity, routes, domains, and connector configuration. --wait observes the resulting state.

Durable tunnels retain identity, routes, connectors, and domains across client sessions. Ephemeral tunnels have a separate lifecycle. Route publication and browser HTTP access require exact authorization; TLS for browser HTTP terminates at the edge.

JSON output is supported with --json.

```
pb tunnel pause <tunnel> [flags]
```

### Options

```
  -h, --help               help for pause
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

