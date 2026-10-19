## pb tunnel rename

Rename a durable tunnel

### Synopsis

Rename a durable tunnel selected by its exact ID or current name. The command reads the current resource version before changing its name and reports the server's canonical result. The name is a label: the tunnel's public address never changes. Existing routes, connectors and access policy remain attached to the same tunnel. Refresh and retry if another writer changed it.

JSON output is supported with --json.

```
pb tunnel rename <tunnel> <name> [flags]
```

### Options

```
  -h, --help               help for rename
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

