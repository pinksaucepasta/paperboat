## pb tunnel connector revoke

Revoke a tunnel connector

### Synopsis

Revoke a tunnel connector

JSON output is supported with --json.

```
pb tunnel connector revoke <tunnel> <connector> [flags]
```

### Options

```
  -h, --help               help for revoke
      --json               print canonical JSON
      --timeout duration   maximum time to wait for operation completion (default 2m0s)
      --wait               wait for the operation to reach a terminal state
      --yes                confirm permanent connector revocation
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --ephemeral          use the temporary preview lifecycle
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb tunnel connector](pb_tunnel_connector.md)	 - Manage tunnel connectors

