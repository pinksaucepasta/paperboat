## pb tunnel policy

Manage permission to activate a private port on demand

### Synopsis

Manage on-demand private or team access to an exact host port. A replacement application listening on the same approved port inherits access. Paperboat does not start the application.

Inspect, allow, or revoke exact on-demand private port policies. A policy authorizes a target port but does not publish it as a public tunnel.

Durable tunnels retain identity, routes, connectors, and domains across client sessions. Ephemeral tunnels have a separate lifecycle. Route publication and browser HTTP access require exact authorization; TLS for browser HTTP terminates at the edge.

With --json, this command group lists its available commands.

### Options

```
  -h, --help   help for policy
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
* [pb tunnel policy allow](pb_tunnel_policy_allow.md)	 - Allow on-demand access to an exact machine port
* [pb tunnel policy get](pb_tunnel_policy_get.md)	 - Show an on-demand port policy
* [pb tunnel policy revoke](pb_tunnel_policy_revoke.md)	 - Revoke an on-demand port policy

