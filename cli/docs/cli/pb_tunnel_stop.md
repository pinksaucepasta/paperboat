## pb tunnel stop

Stop an ephemeral tunnel

### Synopsis

Stop a temporary preview (the ephemeral lifecycle) by its preview ID. A durable tunnel's ID
(tunnel_...) is refused: use pb tunnel pause or pb tunnel delete for those.

JSON output is supported with --json.

```
pb tunnel stop <preview> [flags]
```

### Examples

```
  pb tunnel stop preview_abc123
  pb tunnel --ephemeral stop preview_abc123
```

### Options

```
  -h, --help   help for stop
      --json   print the stopped canonical resource as JSON
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

