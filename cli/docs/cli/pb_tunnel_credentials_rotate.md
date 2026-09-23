## pb tunnel credentials rotate

Rotate tunnel connector credentials

### Synopsis

Rotate tunnel connector credentials

JSON output is supported with --json.

```
pb tunnel credentials rotate <tunnel> [flags]
```

### Options

```
  -h, --help               help for rotate
      --json               print canonical JSON
      --timeout duration   maximum time to wait for operation completion (default 2m0s)
      --wait               wait for the operation to reach a terminal state
      --yes                confirm credential rotation
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --ephemeral          use the temporary preview lifecycle
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb tunnel credentials](pb_tunnel_credentials.md)	 - Manage tunnel credentials

