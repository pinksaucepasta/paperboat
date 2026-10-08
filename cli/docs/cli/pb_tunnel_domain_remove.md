## pb tunnel domain remove

Remove a tunnel domain

### Synopsis

Remove a tunnel domain

Remove one domain binding after confirmation. DNS records managed by the user are not silently removed by this command.

Custom domains require DNS ownership verification before they can serve a route. Domain changes are scoped to one tunnel, and verification status is separate from route readiness. The instructions command returns the required authoritative DNS records.

JSON output is supported with --json.

```
pb tunnel domain remove <tunnel> <domain> [flags]
```

### Options

```
      --confirm string     six-character confirmation code from the preview
  -h, --help               help for remove
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
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb tunnel domain](pb_tunnel_domain.md)	 - Manage tunnel domains

