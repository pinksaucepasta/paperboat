## pb tunnel domain remove

Remove a tunnel domain

### Synopsis

Remove a tunnel domain

JSON output is supported with --json.

```
pb tunnel domain remove <tunnel> <domain> [flags]
```

### Options

```
  -h, --help               help for remove
      --json               print canonical JSON
      --timeout duration   maximum time to wait for operation completion (default 2m0s)
      --wait               wait for the operation to reach a terminal state
      --yes                confirm domain-binding removal while preserving user-owned DNS
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --ephemeral          use the temporary preview lifecycle
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb tunnel domain](pb_tunnel_domain.md)	 - Manage tunnel domains

