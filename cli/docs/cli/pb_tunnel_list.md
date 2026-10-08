## pb tunnel list

List durable tunnels

### Synopsis

List durable tunnels

List durable tunnels visible to this account with cursor and limit pagination. Select an exact tunnel for show, status, routes, domains, or lifecycle commands.

Durable tunnels retain identity, routes, connectors, and domains across client sessions. Ephemeral tunnels have a separate lifecycle. Route publication and browser HTTP access require exact authorization; TLS for browser HTTP terminates at the edge.

JSON output is supported with --json.

```
pb tunnel list [flags]
```

### Options

```
      --cursor string   continue a previous page
  -h, --help            help for list
      --json            print canonical JSON
      --limit int       maximum results (1-200) (default 100)
      --owner string    filter owner: mine, shared, or an authorized account ID
      --q string        filter by resource name or ID
      --state string    filter by resource state
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

