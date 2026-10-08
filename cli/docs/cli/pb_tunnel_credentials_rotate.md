## pb tunnel credentials rotate

Rotate tunnel connector credentials

### Synopsis

Rotate tunnel connector credentials

Rotate the selected tunnel's connector credentials and revoke the old authority. --wait observes the operation; the confirmation code confirms a change that may interrupt connectors.

Connector credentials authorize a tunnel attachment. Rotation replaces that authority and may interrupt existing connectors; use the status and connector commands to confirm recovery.

JSON output is supported with --json.

```
pb tunnel credentials rotate <tunnel> [flags]
```

### Options

```
      --confirm string     six-character confirmation code from the preview
  -h, --help               help for rotate
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

* [pb tunnel credentials](pb_tunnel_credentials.md)	 - Manage tunnel credentials

