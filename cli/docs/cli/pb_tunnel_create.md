## pb tunnel create

Create a durable tunnel

### Synopsis

Create a durable tunnel that serves a service on this machine. The machine must be enrolled
first (pb pair). Give the tunnel a name of 1 to 63 letters, digits, ".", "_" or "-",
not starting with punctuation, and its origin with --port (a local TCP port) or --from (an
origin URL: http, https, h2c, tcp or unix).

Access defaults to public. --private requires authenticated private access and --team requires an explicit team grant; they cannot be combined. --domain adds
a custom hostname after creation. By default the tunnel never expires; --duration sets a
lifetime, and pb tunnel extend moves it later.

A change returns once the request is accepted, and the tunnel may still be connecting. Pass
--wait to wait (at most --timeout, default 2m, 1s to 10m) until it finishes, or check pb tunnel
status.

JSON output is supported with --json.

```
pb tunnel create <name> [flags]
```

### Examples

```
  pb tunnel create web --port 8080
  pb tunnel create api --from http://127.0.0.1:9000 --private
  pb tunnel create docs --port 3000 --domain docs.example.com --wait
  pb tunnel create temp --port 8080 --duration 24h
```

### Options

```
      --domain strings      custom domain to add after creation (repeatable)
      --duration duration   tunnel lifetime (default: the tunnel does not expire)
      --from string         origin URL: http, https, h2c, tcp or unix (instead of --port)
  -h, --help                help for create
      --json                print canonical JSON
      --port int            local TCP port to serve (or use --from)
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

