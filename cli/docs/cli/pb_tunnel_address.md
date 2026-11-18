## pb tunnel address

Change a durable tunnel's public address

### Synopsis

Change the public address of a durable tunnel, selected by ID or name. The subdomain becomes
the first label of the address under the same base domain: helloworld makes helloworld.<base domain>.
It is 3 to 63 lowercase letters, digits or hyphens, not starting or ending with a hyphen and not only
digits. Some names are reserved, and a name another tunnel uses is refused with the reason.

The new address is published first and the previous one keeps serving until every Paperboat edge serves the
new one (usually seconds, at most five minutes; a change the edges do not confirm in time is undone and
reported as failed; with --wait pass --timeout 6m for slow edges). Until then the new address may answer
"not found" on some edges. This holds for the managed HTTP address; a TLS or TCP route, or a tunnel nothing
serves yet, moves at once. Then the previous address stops working and stays reserved to the tunnel for a
cooldown (a day by default) before anyone else may take it, so update every link, webhook and bookmark that
uses it. Routes, connectors and access rules stay with the tunnel. Address changes are rate limited per account.

A change returns once the request is accepted, and the tunnel may still be connecting. Pass
--wait to wait (at most --timeout, default 2m, 1s to 10m) until it finishes, or check pb tunnel
status.

JSON output is supported with --json.

```
pb tunnel address <tunnel> <subdomain> [flags]
```

### Examples

```
  pb tunnel address web helloworld
  pb tunnel address tunnel_abc123 helloworld --wait
```

### Options

```
  -h, --help               help for address
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

* [pb tunnel](pb_tunnel.md)	 - Manage durable tunnels or start an ephemeral tunnel

