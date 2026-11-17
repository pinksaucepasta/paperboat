## pb tunnel delete

Delete endpoints and routes while preserving user DNS records

### Synopsis

Delete a durable tunnel with its endpoints, routes and domain bindings. DNS records you keep
at your own provider are not touched, and audit history stays. With --ephemeral, stop a
temporary preview instead.

Confirmation is two steps. Run the command without --confirm: it changes nothing, shows what
would happen and prints a six-character code (exit status 2). Run the same command again with
--confirm CODE within 5 minutes. The code works once and only for that exact command and target.

JSON output is supported with --json.

```
pb tunnel delete <tunnel> [flags]
```

### Examples

```
  pb tunnel delete web
  pb tunnel delete web --confirm A1B2C3 --json
```

### Options

```
      --confirm string     six-character confirmation code from the preview
  -h, --help               help for delete
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

