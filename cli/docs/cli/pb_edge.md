## pb edge

Inspect tunnel edges

### Synopsis

Inspect the tunnel edges that serve the active workspace: edges Paperboat runs, or the
self-hosted edges you selected. Listing an edge never grants a route admission.

With --json, this command group lists its available commands.

```
pb edge [flags]
```

### Examples

```
  pb edge list
  pb edge list --workspace acme --json
```

### Options

```
  -h, --help   help for edge
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal
* [pb edge list](pb_edge_list.md)	 - List managed or selected self-hosted tunnel edges

