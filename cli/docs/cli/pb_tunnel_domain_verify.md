## pb tunnel domain verify

Verify a tunnel domain

### Synopsis

Verify a tunnel domain

Check DNS ownership for one registered domain and update its verification state. --wait observes the operation; serving still depends on route and connector readiness.

Custom domains require DNS ownership verification before they can serve a route. Domain changes are scoped to one tunnel, and verification status is separate from route readiness. The instructions command returns the required authoritative DNS records.

JSON output is supported with --json.

```
pb tunnel domain verify <tunnel> <domain> [flags]
```

### Options

```
  -h, --help               help for verify
      --json               print canonical JSON
      --timeout duration   maximum time to wait for operation completion (default 2m0s)
      --wait               wait for the operation to reach a terminal state
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --ephemeral          use the temporary preview lifecycle
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb tunnel domain](pb_tunnel_domain.md)	 - Manage tunnel domains

