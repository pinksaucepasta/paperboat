## pb tunnel route

Manage tunnel routes

### Synopsis

List, add, update or remove the routes of a durable tunnel. A route ties a protocol, a match
(a hostname and an optional path prefix) and an origin together, and a tunnel can have several.
Each change may return an operation that is still connecting; see pb tunnel status.

With --json, this command group lists its available commands.

```
pb tunnel route [flags]
```

### Examples

```
  pb tunnel route list web
  pb tunnel route add web --name app --to http://127.0.0.1:8080 --domain app.example.com
```

### Options

```
  -h, --help   help for route
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --ephemeral          use the temporary preview lifecycle
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb tunnel](pb_tunnel.md)	 - Manage durable tunnels or start an ephemeral tunnel
* [pb tunnel route add](pb_tunnel_route_add.md)	 - Add a tunnel route
* [pb tunnel route list](pb_tunnel_route_list.md)	 - List tunnel routes
* [pb tunnel route remove](pb_tunnel_route_remove.md)	 - Remove a tunnel route
* [pb tunnel route update](pb_tunnel_route_update.md)	 - Update a tunnel route

