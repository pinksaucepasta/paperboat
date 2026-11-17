## pb tunnel route update

Update a tunnel route

### Synopsis

Change the fields you name on one route; the rest stay as they are. The flags are the ones of
route add. --clear-* flags remove a single override (the Host override, path prefix, TLS server
name, header rules, redirects or rewrite), and --enable or --disable switches the route on or
off. Unlike add, update accepts only http and tcp_private for --protocol.

A change returns once the request is accepted, and the tunnel may still be connecting. Pass
--wait to wait (at most --timeout, default 2m, 1s to 10m) until it finishes, or check pb tunnel
status.

JSON output is supported with --json.

```
pb tunnel route update <tunnel> <route> [flags]
```

### Examples

```
  pb tunnel route update web route_xyz --to http://127.0.0.1:8081
  pb tunnel route update web route_xyz --disable
  pb tunnel route update web route_xyz --clear-host-header --wait
```

### Options

```
      --clear-client-credential-reference    remove the origin mTLS credential reference
      --clear-host-header                    remove the origin Host override
      --clear-path                           remove the HTTP path prefix
      --clear-redirects                      remove every redirect
      --clear-request-headers                remove every request header rule
      --clear-response-headers               remove every response header rule
      --clear-rewrite-path                   remove the path rewrite
      --clear-tls-server-name                remove the explicit origin TLS server name
      --client-credential-reference string   protected origin mTLS credential reference
      --connect-timeout duration             new origin connect timeout
      --disable                              disable the route
      --domain string                        new exact hostname, wildcard, or empty catch-all
      --enable                               enable the route
  -h, --help                                 help for update
      --host-header string                   override the origin Host header
      --idle-timeout duration                new stream idle timeout
      --json                                 print canonical JSON
      --max-streams int32                    new maximum concurrent streams
      --name string                          new route name
      --path string                          new HTTP path prefix
      --preserve-host                        preserve the incoming Host header (default true)
      --priority int32                       new route priority
      --protocol string                      new route protocol (http or tcp_private)
      --redirect stringArray                 redirect a path prefix: '/from -> [301|302|307|308:]/to-or-https-URL' (repeatable, at most 4)
      --request-header stringArray           request header rule op:Name=value (op add or set) or remove:Name, sent to the app (repeatable, at most 8)
      --response-header stringArray          response header rule op:Name=value (op add or set) or remove:Name, sent to the visitor (repeatable, at most 8)
      --rewrite-path stringArray             rewrite a path prefix before the app sees it: '/from -> /to'
      --timeout duration                     maximum time to wait for operation completion (default 2m0s)
      --tls-ca string                        trust this CA for the origin (PEM file, e.g. Caddy's tls internal root); sets custom_ca
      --tls-pin string                       pin the origin certificate by its fingerprint (sha256:<hex>), without connecting
      --tls-server-name string               explicit origin TLS server name
      --tls-verification string              origin TLS verification: system, custom_ca, pinned (shows the origin certificate and pins it), or insecure
      --to string                            new origin URL
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

