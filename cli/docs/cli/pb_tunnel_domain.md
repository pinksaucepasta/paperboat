## pb tunnel domain

Manage tunnel domains

### Synopsis

Manage tunnel domains

Manage custom domains bound to a durable tunnel. Add the hostname, follow authoritative DNS instructions, verify ownership, then inspect route readiness.

Durable tunnels retain identity, routes, connectors, and domains across client sessions. Ephemeral tunnels have a separate lifecycle. Route publication and browser HTTP access require exact authorization; TLS for browser HTTP terminates at the edge.

With --json, this command group lists its available commands.

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
* [pb tunnel domain add](pb_tunnel_domain_add.md)	 - Add a tunnel domain
* [pb tunnel domain instructions](pb_tunnel_domain_instructions.md)	 - Show authoritative DNS instructions for a tunnel domain
* [pb tunnel domain list](pb_tunnel_domain_list.md)	 - List tunnel domains
* [pb tunnel domain remove](pb_tunnel_domain_remove.md)	 - Remove a tunnel domain
* [pb tunnel domain verify](pb_tunnel_domain_verify.md)	 - Verify a tunnel domain

