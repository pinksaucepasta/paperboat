## pb tunnel show

Show a durable tunnel

### Synopsis

Show one durable tunnel's identity, address, access mode and desired state. Name it by ID or
exact name; if a name is ambiguous, retry with the tunnel's ID. --json prints the whole
resource. For current health use pb tunnel status; for history use pb tunnel logs.

JSON output is supported with --json.

```
pb tunnel show <tunnel> [flags]
```

### Examples

```
  pb tunnel show web
  pb tunnel show tunnel_abc123 --json
```

### Options

```
  -h, --help   help for show
      --json   print canonical JSON
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

