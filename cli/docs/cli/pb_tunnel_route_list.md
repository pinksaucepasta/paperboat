## pb tunnel route list

List tunnel routes

### Synopsis

List tunnel routes

List routes and their identifiers for one durable tunnel with pagination. Inspect the exact route before updating or removing it.

Routes map a public or private match to a specific origin and protocol. TLS and host-header options affect the trust boundary and should match the origin. Adding or updating a route may return an operation whose readiness must be checked separately.

JSON output is supported with --json.

```
pb tunnel route list <tunnel> [flags]
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

* [pb tunnel route](pb_tunnel_route.md)	 - Manage tunnel routes

