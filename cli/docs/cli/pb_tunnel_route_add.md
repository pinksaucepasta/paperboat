## pb tunnel route add

Add a tunnel route

### Synopsis

Add a route to a durable tunnel. --name and --to are required: --to is the origin URL the route forwards to. --domain is
the hostname it matches (an exact name or a one-label wildcard) and --path an optional HTTP path
prefix, which may end in *. --protocol is http (the default), tcp, tls passthrough or
tcp_private.

Origin options: --host-header or --preserve-host choose the Host the app sees; --tls-verification
(system, custom_ca, pinned or insecure) with --tls-ca, --tls-pin and --tls-server-name decide
how the origin's certificate is trusted; --connect-timeout (default 10s), --idle-timeout
(default 5m) and --max-streams (default 1024) bound the connection. Header, redirect and
path-rewrite rules are repeatable (--request-header and --response-header up to 8, --redirect up
to 4).

A change returns once the request is accepted, and the tunnel may still be connecting. Pass
--wait to wait (at most --timeout, default 2m, 1s to 10m) until it finishes, or check pb tunnel
status.

JSON output is supported with --json.

```
pb tunnel route add <tunnel> [flags]
```

### Examples

```
  pb tunnel route add web --name app --to http://127.0.0.1:8080 --domain app.example.com
  pb tunnel route add web --name db --protocol tcp --to tcp://127.0.0.1:5432 --domain db.example.com
  pb tunnel route add web --name docs --to http://127.0.0.1:3000 --domain app.example.com --path /docs --wait
```

### Options

```
      --clear-client-credential-reference    remove the origin mTLS credential reference
      --clear-host-header                    remove the origin Host override
      --clear-tls-server-name                remove the explicit origin TLS server name
      --client-credential-reference string   protected origin mTLS credential reference
      --connect-timeout duration             origin connect timeout (default 10s)
      --domain string                        exact hostname or one-label wildcard match
  -h, --help                                 help for add
      --host-header string                   override the origin Host header
      --idle-timeout duration                stream idle timeout (default 5m0s)
      --json                                 print canonical JSON
      --max-streams int32                    maximum concurrent streams (default 1024)
      --name string                          route name (required)
      --path string                          HTTP path prefix, optionally ending in *
      --preserve-host                        preserve the incoming Host header (default true)
      --priority int32                       route priority
      --protocol string                      route protocol (http, tcp, tls passthrough, or tcp_private) (default "http")
      --redirect stringArray                 redirect a path prefix: '/from -> [301|302|307|308:]/to-or-https-URL' (repeatable, at most 4)
      --request-header stringArray           request header rule op:Name=value (op add or set) or remove:Name, sent to the app (repeatable, at most 8)
      --response-header stringArray          response header rule op:Name=value (op add or set) or remove:Name, sent to the visitor (repeatable, at most 8)
      --rewrite-path stringArray             rewrite a path prefix before the app sees it: '/from -> /to'
      --timeout duration                     maximum time to wait for operation completion (default 2m0s)
      --tls-ca string                        trust this CA for the origin (PEM file, e.g. Caddy's tls internal root); sets custom_ca
      --tls-pin string                       pin the origin certificate by its fingerprint (sha256:<hex>), without connecting
      --tls-server-name string               explicit origin TLS server name
      --tls-verification string              origin TLS verification: system, custom_ca, pinned (shows the origin certificate and pins it), or insecure
      --to string                            origin URL the route forwards to (required)
      --wait                                 wait for the operation to reach a terminal state
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

* [pb tunnel route](pb_tunnel_route.md)	 - Manage tunnel routes

