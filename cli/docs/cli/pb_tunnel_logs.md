## pb tunnel logs

Show tunnel logs

### Synopsis

Show tunnel logs

Read recent tunnel events and optionally follow new events at the requested interval. Cursor and limit bound retrieval; logs are diagnostic rather than a substitute for status.

Durable tunnels retain identity, routes, connectors, and domains across client sessions. Ephemeral tunnels have a separate lifecycle. Route publication and browser HTTP access require exact authorization; TLS for browser HTTP terminates at the edge.

JSON output is supported with --json.

```
pb tunnel logs <tunnel> [flags]
```

### Options

```
      --cursor string       continue after a log cursor
      --follow              wait for new log entries
  -h, --help                help for logs
      --interval duration   follow polling interval (250ms-1m) (default 1s)
      --json                print canonical JSON
      --limit int           maximum entries per request (1-200) (default 100)
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

