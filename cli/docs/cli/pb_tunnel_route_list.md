## pb tunnel route list

List tunnel routes

### Synopsis

List a durable tunnel's routes: ID, name, protocol, match, origin and state. Use the route ID
with route update, route remove, domain add and inspect. Page with --cursor and --limit.

JSON output is supported with --json.

```
pb tunnel route list <tunnel> [flags]
```

### Examples

```
  pb tunnel route list web
  pb tunnel route list web --limit 50 --json
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

