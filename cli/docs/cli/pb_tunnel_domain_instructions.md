## pb tunnel domain instructions

Show authoritative DNS instructions for a tunnel domain

### Synopsis

Show authoritative DNS instructions for a tunnel domain

Show the authoritative DNS records required for one tunnel domain. Apply them at the domain's DNS provider before running verify.

Custom domains require DNS ownership verification before they can serve a route. Domain changes are scoped to one tunnel, and verification status is separate from route readiness. The instructions command returns the required authoritative DNS records.

JSON output is supported with --json.

```
pb tunnel domain instructions <tunnel> <domain> [flags]
```

### Options

```
  -h, --help   help for instructions
      --json   print canonical JSON
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

