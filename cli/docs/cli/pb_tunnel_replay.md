## pb tunnel replay

Deliberately replay one retained HTTP request to the same origin

### Synopsis

Send one captured HTTP request again to the same tunnel origin. This repeats whatever the
request did, including side effects such as payments or writes. Name the capture by its ID
(from pb tunnel inspect) and pass --route when the tunnel has several routes. An idempotency
key is generated unless you pass --idempotency-key; reuse one key to retry safely.

JSON output is supported with --json.

```
pb tunnel replay <tunnel> <capture-id> [flags]
```

### Examples

```
  pb tunnel replay web cap_123
  pb tunnel replay web cap_123 --route route_xyz --idempotency-key order-42
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
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb tunnel](pb_tunnel.md)	 - Manage durable tunnels or start an ephemeral tunnel

