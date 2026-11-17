## pb tunnel policy

Manage permission to activate a private port on demand

### Synopsis

Manage permission to open a private port on a machine when someone asks for it, without
making a tunnel first. A policy authorizes one exact machine port for private or team access. It
never publishes a public URL and never opens other ports of the machine. Revoking a policy
blocks later activations. If the application behind an approved port is replaced, the new one
inherits access; Paperboat does not start the application.

With --json, this command group lists its available commands.

```
pb tunnel policy [flags]
```

### Examples

```
  pb tunnel policy allow homelab 3000
  pb tunnel policy list
  pb tunnel policy revoke pol_abc
```

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
* [pb tunnel policy list](pb_tunnel_policy_list.md)	 - List on-demand port policies (their IDs and generations)
* [pb tunnel policy revoke](pb_tunnel_policy_revoke.md)	 - Revoke an on-demand port policy

