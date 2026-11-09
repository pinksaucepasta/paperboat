## pb tunnel domain add

Serve a tunnel route at a hostname under a verified workspace domain (pb domain add)

### Synopsis

Serve a tunnel route at a hostname under a verified workspace domain (pb domain add)

Serve one tunnel route at a hostname under a verified workspace domain, such as app.example.com under example.com. HTTPS comes from the domain's certificate.

Hostnames come from the workspace's verified domains (pb domain). A hostname is one label under a domain, or the domain itself; the domain's verification and certificate cover it.

JSON output is supported with --json.

```
pb tunnel domain add <tunnel> <hostname> [flags]
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

