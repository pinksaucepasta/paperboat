## pb tunnel pause

Pause new traffic while preserving tunnel identity and configuration

### Synopsis

Pause new traffic while preserving tunnel identity and configuration

JSON output is supported with --json.

```
pb tunnel pause <tunnel> [flags]
```

### Options

```
  -h, --help               help for pause
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
```

### SEE ALSO

* [pb tunnel](pb_tunnel.md)	 - Manage durable tunnels or start an ephemeral tunnel

