## pb tunnel route update

Update a tunnel route

### Synopsis

Update a tunnel route

Change explicit fields of one route, including origin, match, protocol, or TLS options. Clear flags remove individual overrides; check the resulting operation and status before assuming traffic moved.

Routes map a public or private match to a specific origin and protocol. TLS and host-header options affect the trust boundary and should match the origin. Adding or updating a route may return an operation whose readiness must be checked separately.

JSON output is supported with --json.

```
pb tunnel route update <tunnel> <route> [flags]
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
      --rewrite-path string                  rewrite a path prefix before the app sees it: '/from -> /to'
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

