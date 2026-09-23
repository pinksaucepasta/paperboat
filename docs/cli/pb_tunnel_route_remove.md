## pb tunnel route remove

Remove a tunnel route

### Synopsis

Remove a tunnel route

JSON output is supported with --json.

```
pb tunnel route remove <tunnel> <route> [flags]
```

### Options

```
  -h, --help               help for remove
      --json               print canonical JSON
      --timeout duration   maximum time to wait for operation completion (default 2m0s)
      --wait               wait for the operation to reach a terminal state
      --yes                confirm route removal while preserving the tunnel
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --ephemeral          use the temporary preview lifecycle
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb tunnel route](pb_tunnel_route.md)	 - Manage tunnel routes

