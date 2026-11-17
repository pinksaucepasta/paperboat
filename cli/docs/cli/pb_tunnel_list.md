## pb tunnel list

List durable tunnels

### Synopsis

List durable tunnels you can see: ID, name, address, access mode and state. Filter with --q
(name or ID), --state and --owner (mine, shared or an authorized account ID); page with
--cursor and --limit (1-200, default 100). With --ephemeral the list shows temporary previews
instead, and --all adds ones that ended in the last 30 days.

JSON output is supported with --json.

```
pb tunnel list [flags]
```

### Examples

```
  pb tunnel list
  pb tunnel list --state active --limit 50
  pb tunnel list --owner mine --json
  pb tunnel --ephemeral list --all
```

### Options

```
      --all             with --ephemeral, include ended previews (kept 30 days after they end)
      --cursor string   continue a previous page
  -h, --help            help for list
      --json            print canonical JSON
      --limit int       maximum results (1-200) (default 100)
      --owner string    filter owner: mine, shared, or an authorized account ID
      --q string        filter by resource name or ID
      --state string    filter by resource state
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

