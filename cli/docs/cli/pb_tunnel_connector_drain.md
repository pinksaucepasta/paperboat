## pb tunnel connector drain

Drain a tunnel connector

### Synopsis

Drain a tunnel connector

Stop assigning new work to one connector and optionally wait for existing work to finish. --timeout bounds the wait; the connector remains distinct from a revoked one.

Connectors attach an enrolled host to a durable tunnel. Drain stops new work while allowing existing work to wind down; revoke removes connector authority. Inspect connector state before replacing or removing one.

JSON output is supported with --json.

```
pb tunnel connector drain <tunnel> <connector> [flags]
```

### Options

```
  -h, --help               help for drain
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

* [pb tunnel connector](pb_tunnel_connector.md)	 - Manage tunnel connectors

