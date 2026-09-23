## pb tunnel replay

Deliberately replay one retained HTTP request to the same origin

### Synopsis

Deliberately replay one retained HTTP request to the same origin

JSON output is supported with --json.

```
pb tunnel replay <tunnel> <capture-id> [flags]
```

### Options

```
  -h, --help                     help for replay
      --idempotency-key string   explicit idempotency key for safe retry (generated when omitted)
      --json                     print canonical JSON
      --route string             replay a capture from one route ID (required when the tunnel has several routes)
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

