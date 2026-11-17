## pb tunnel domain add

Serve a tunnel route at a hostname under a verified workspace domain (pb domain add)

### Synopsis

Serve one route of a tunnel at a hostname under a verified workspace domain: for example
app.example.com when example.com is added and verified with pb domain add and pb domain verify.
The domain's verification and certificate cover the hostname, so there is no DNS step of its
own. --route picks the route the hostname serves.

A change returns once the request is accepted, and the tunnel may still be connecting. Pass
--wait to wait (at most --timeout, default 2m, 1s to 10m) until it finishes, or check pb tunnel
status.

JSON output is supported with --json.

```
pb tunnel domain add <tunnel> <hostname> [flags]
```

### Examples

```
  pb tunnel domain add web app.example.com --route route_xyz
  pb tunnel domain add web app.example.com --route route_xyz --wait
```

### Options

```
  -h, --help               help for add
      --json               print canonical JSON
      --route string       route to serve at the hostname
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

* [pb tunnel domain](pb_tunnel_domain.md)	 - Serve a tunnel at hostnames under your workspace's domains

