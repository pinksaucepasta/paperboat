## pb tunnel route update

Update a tunnel route

### Synopsis

Update a tunnel route

JSON output is supported with --json.

```
pb tunnel route update <tunnel> <route> [flags]
```

### Options

```
      --ca-reference string                  protected custom CA reference
      --clear-ca-reference                   remove the custom CA reference
      --clear-client-credential-reference    remove the origin mTLS credential reference
      --clear-host-header                    remove the origin Host override
      --clear-path                           remove the HTTP path prefix
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
      --timeout duration                     maximum time to wait for operation completion (default 2m0s)
      --tls-server-name string               explicit origin TLS server name
      --tls-verification string              origin TLS verification (system, custom_ca, or insecure_development)
      --to string                            new origin URL
      --wait                                 wait for the operation to reach a terminal state
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --ephemeral          use the temporary preview lifecycle
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb tunnel route](pb_tunnel_route.md)	 - Manage tunnel routes

