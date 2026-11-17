## pb tunnel route remove

Remove a tunnel route

### Synopsis

Remove one route from a durable tunnel. The tunnel and its other routes keep serving.

Confirmation is two steps. Run the command without --confirm: it changes nothing, shows what
would happen and prints a six-character code (exit status 2). Run the same command again with
--confirm CODE within 5 minutes. The code works once and only for that exact command and target.

JSON output is supported with --json.

```
pb tunnel route remove <tunnel> <route> [flags]
```

### Examples

```
  pb tunnel route remove web route_xyz
  pb tunnel route remove web route_xyz --confirm A1B2C3
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

* [pb tunnel route](pb_tunnel_route.md)	 - Manage tunnel routes

