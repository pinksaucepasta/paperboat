## pb tunnel domain list

List tunnel domains

### Synopsis

List tunnel domains

List domains for one durable tunnel with pagination. The returned state distinguishes registration from verified ownership and serving readiness.

Custom domains require DNS ownership verification before they can serve a route. Domain changes are scoped to one tunnel, and verification status is separate from route readiness. The instructions command returns the required authoritative DNS records.

JSON output is supported with --json.

```
pb tunnel domain list <tunnel> [flags]
```

### Options

```
      --cursor string   continue a previous page
  -h, --help            help for list
      --json            print canonical JSON
      --limit int       maximum results (1-200) (default 100)
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

