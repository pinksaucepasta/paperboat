## pb tunnel domain remove

Stop serving a tunnel at a hostname

### Synopsis

Stop serving a tunnel at a hostname

Stop serving one tunnel at a hostname after confirmation. The workspace domain and its DNS records stay.

Hostnames come from the workspace's verified domains (pb domain). A hostname is one label under a domain, or the domain itself; the domain's verification and certificate cover it.

JSON output is supported with --json.

```
pb tunnel domain remove <tunnel> <hostname> [flags]
```

### Options

```
      --confirm string     six-character confirmation code from the preview
  -h, --help               help for remove
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

* [pb tunnel domain](pb_tunnel_domain.md)	 - Serve a tunnel at hostnames under your workspace's domains

