## pb tunnel domain add

Add a tunnel domain

### Synopsis

Add a tunnel domain

Register a hostname for one tunnel or route. Registration alone does not prove ownership or make traffic ready; use instructions and verify to complete DNS setup.

Custom domains require DNS ownership verification before they can serve a route. Domain changes are scoped to one tunnel, and verification status is separate from route readiness. The instructions command returns the required authoritative DNS records.

JSON output is supported with --json.

```
pb tunnel domain add <tunnel> <hostname> [flags]
```

### Options

```
  -h, --help               help for add
      --json               print canonical JSON
      --provider string    DNS provider (default "generic")
      --route string       route
      --timeout duration   maximum time to wait for operation completion (default 2m0s)
      --wait               wait for the operation to reach a terminal state
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

* [pb tunnel domain](pb_tunnel_domain.md)	 - Manage tunnel domains

