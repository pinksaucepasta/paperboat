## pb tunnel inspect

Show daemon-local HTTP captures

### Synopsis

Show daemon-local HTTP captures

Read or manage bounded HTTP captures for a tunnel, optionally filtered by route. --body and --raw expose retained content; enable, disable, and purge change capture state.

Durable tunnels retain identity, routes, connectors, and domains across client sessions. Ephemeral tunnels have a separate lifecycle. Route publication and browser HTTP access require exact authorization; TLS for browser HTTP terminates at the edge.

JSON output is supported with --json.

```
pb tunnel inspect <tunnel> [flags]
```

### Options

```
      --body            capture redacted request/response bodies with --enable
      --cursor string   continue after a capture cursor
      --disable         disable capture for this resource
      --enable          enable capture for this resource
  -h, --help            help for inspect
      --id string       show one capture by ID
      --json            print canonical JSON
      --limit int       maximum captures per request (1-100) (default 20)
      --purge           purge retained captures for this resource
      --raw             retain exact raw requests for replay with --enable
      --route string    inspect one route ID (required when the tunnel has several routes)
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

