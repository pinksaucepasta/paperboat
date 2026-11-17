## pb tunnel status

Show tunnel health

### Synopsis

Show current health for one durable tunnel, by ID or name. A route that is configured is not
necessarily ready to serve. --watch keeps refreshing every --interval (250ms to 1m, default 1s)
until interrupted. With --ephemeral it shows a temporary preview's status instead.

JSON output is supported with --json.

```
pb tunnel status <tunnel> [flags]
```

### Examples

```
  pb tunnel status web
  pb tunnel status web --watch --interval 5s
  pb tunnel status web --json
```

### Options

```
  -h, --help                help for status
      --interval duration   watch polling interval (250ms-1m) (default 1s)
      --json                print canonical JSON
      --watch               watch for health changes
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

