## pb tunnel create

Create a durable tunnel

### Synopsis

Create a durable tunnel

Create a durable tunnel with a local port or origin and optional domain, privacy, and team settings. The returned operation may still be connecting; --wait observes readiness within --timeout.

Durable tunnels retain identity, routes, connectors, and domains across client sessions. Ephemeral tunnels have a separate lifecycle. Route publication and browser HTTP access require exact authorization; TLS for browser HTTP terminates at the edge.

JSON output is supported with --json.

```
pb tunnel create <name> [flags]
```

### Options

```
      --domain strings      custom domain to add after creation
      --duration duration   optional tunnel lifetime
      --from string         origin URL (http, https, h2c, tcp, or unix)
  -h, --help                help for create
      --json                print canonical JSON
      --port int            local TCP port
      --private             require authenticated private access
      --team                require an explicit team grant for browser access
      --timeout duration    maximum time to wait for operation completion (default 2m0s)
      --wait                wait for the operation to reach a terminal state
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

