## pb tunnel extend

Move a durable tunnel's expiry to now plus a duration

### Synopsis

Set a durable tunnel's expiry to the current time plus --duration, on the tunnel's current resource version. Use it when `pb tunnel status` says the tunnel expired; the tunnel serves again once the change is applied.

JSON output is supported with --json.

```
pb tunnel extend <tunnel> --duration <time> [flags]
```

### Options

```
      --duration duration   new lifetime from now, for example 24h
  -h, --help                help for extend
      --json                print canonical JSON
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

