## pb tunnel stop

Stop an ephemeral tunnel

### Synopsis

Stop an ephemeral tunnel

Stop an ephemeral tunnel or preview by its identity. Durable tunnels have pause, resume, and delete commands with different lifecycle effects.

Durable tunnels retain identity, routes, connectors, and domains across client sessions. Ephemeral tunnels have a separate lifecycle. Route publication and browser HTTP access require exact authorization; TLS for browser HTTP terminates at the edge.

JSON output is supported with --json.

```
pb tunnel stop <preview> [flags]
```

### Options

```
  -h, --help   help for stop
      --json   print the stopped canonical resource as JSON
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

