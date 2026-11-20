## pb edge list

List the regions tunnels and previews are served from

### Synopsis

List the regions tunnels and previews are served from for the active
workspace. Managed-only mode lists Paperboat's regions. Self-hosted-only mode
lists only your own servers, one row per server under the region you gave it,
including ones that are currently unavailable. It never falls back to
Paperboat's regions. Self-hosted browser publications require a verified
customer domain and a ready certificate.

Listing a region never guarantees admission for a particular route; domain and
route authorization still apply.

The table identifies each region's name, source and status. A region is ready
while any of its servers is. Use --json for paperboat.edge-list/v1 output with
the active workspace's pool mode and region list.

JSON output is supported with --json.

```
pb edge list [flags]
```

### Examples

```
  pb edge list
  pb edge list --workspace acme
  pb edge list --json
```

### Options

```
  -h, --help   help for list
      --json   print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb edge](pb_edge.md)	 - Inspect the regions tunnels and previews are served from

