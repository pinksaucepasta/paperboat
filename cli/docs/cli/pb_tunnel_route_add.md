## pb tunnel route add

Add a tunnel route

### Synopsis

Add a tunnel route

Add a route to one durable tunnel, selecting protocol, match, origin, and optional TLS behavior. The operation can remain connecting after the command returns; use --wait or status to confirm readiness.

Routes map a public or private match to a specific origin and protocol. TLS and host-header options affect the trust boundary and should match the origin. Adding or updating a route may return an operation whose readiness must be checked separately.

JSON output is supported with --json.

```
pb tunnel route add <tunnel> [flags]
```

### Options

```
      --ca-reference string                  protected custom CA reference
      --clear-ca-reference                   remove the custom CA reference
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
      --max-streams int32                    maximum concurrent streams (default 128)
      --name string                          name
      --path string                          HTTP path prefix, optionally ending in *
      --preserve-host                        preserve the incoming Host header (default true)
      --priority int32                       route priority
      --protocol string                      route protocol (http, tcp, or tcp_private) (default "http")
      --timeout duration                     maximum time to wait for operation completion (default 2m0s)
      --tls-server-name string               explicit origin TLS server name
      --tls-verification string              origin TLS verification (system, custom_ca, or insecure_development)
      --to string                            to
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

