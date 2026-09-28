## pb edge list

List hosted and self-hosted tunnel edges

### Synopsis

List tunnel edge nodes visible to the signed-in account. Paperboat-hosted
edges and self-hosted edges selected in the account's tunnel pool appear in
mixed mode. Self-hosted-only mode shows selected self-hosted edges, including
ones that are currently unavailable.

If self-hosted-only mode has no selected active edge, hosted edges are shown
for reference. They cannot serve routes until the tunnel pool changes to
mixed mode. Listing an edge never guarantees admission for a particular route;
domain and route authorization still apply.

The table identifies each edge's source, region, status, and last heartbeat.
Self-hosted readiness comes from the control plane; hosted status is an
observed node state. Use --json for paperboat.edge-list/v1 output with the
pool mode, edges, and hosted_fallback_display_only marker.

JSON output is supported with --json.

```
pb edge list [flags]
```

### Examples

```
  pb edge list
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
```

### SEE ALSO

* [pb edge](pb_edge.md)	 - Inspect tunnel edges

