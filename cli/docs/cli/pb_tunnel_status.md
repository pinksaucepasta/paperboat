## pb tunnel status

Show tunnel health

### Synopsis

Show tunnel health

Show current health for one durable tunnel. --watch refreshes observations at --interval; a configured route is not necessarily ready to serve traffic.

Durable tunnels retain identity, routes, connectors, and domains across client sessions. Ephemeral tunnels have a separate lifecycle. Route publication and browser HTTP access require exact authorization; TLS for browser HTTP terminates at the edge.

JSON output is supported with --json.

```
pb tunnel status <tunnel> [flags]
```

### Options

```
  -h, --help                help for status
      --interval duration   watch polling interval (250ms-1m) (default 1s)
      --json                print canonical JSON
      --watch               watch for health changes
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

