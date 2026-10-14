## pb edge list

List hosted and self-hosted tunnel edges

### Synopsis

List tunnel edge nodes in the active workspace. Managed-only mode lists
Paperboat-managed edges. Self-hosted-only mode lists only selected self-hosted
edges, including ones that are currently unavailable, and never falls back to
managed edges. Self-hosted browser publications require a verified customer
domain and a ready certificate. Listing an edge never guarantees admission
for a particular route; domain and route authorization still apply.

The table identifies each edge's source, region, status, and last heartbeat.
Self-hosted readiness comes from the control plane; hosted status is an
observed node state. Use --json for paperboat.edge-list/v1 output with the
active workspace's pool mode and edge list.

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

* [pb edge](pb_edge.md)	 - Inspect tunnel edges

