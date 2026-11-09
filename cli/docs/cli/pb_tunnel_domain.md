## pb tunnel domain

Serve a tunnel at hostnames under your workspace's domains

### Synopsis

Serve a tunnel at hostnames under your workspace's domains

Serve a durable tunnel at your own hostnames. A hostname must be one level under a verified workspace domain (pb domain add); it needs no DNS step of its own.

Durable tunnels retain identity, routes, and domains across client sessions. Ephemeral tunnels have a separate lifecycle. Route publication and browser HTTP access require exact authorization; TLS for browser HTTP terminates at the edge.

With --json, this command group lists its available commands.

```
pb tunnel domain [flags]
```

### Options

```
  -h, --help   help for domain
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
* [pb tunnel domain add](pb_tunnel_domain_add.md)	 - Serve a tunnel route at a hostname under a verified workspace domain (pb domain add)
* [pb tunnel domain list](pb_tunnel_domain_list.md)	 - List tunnel domains
* [pb tunnel domain remove](pb_tunnel_domain_remove.md)	 - Stop serving a tunnel at a hostname

