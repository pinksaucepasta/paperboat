## pb edge

Inspect tunnel edges

### Synopsis

Inspect tunnel edges

List edges in the active workspace according to its tunnel pool. Managed-only lists Paperboat-managed edges; self-hosted-only lists only the selected self-hosted edges and never falls back to managed edges. Self-hosted browser publications require a verified customer domain and ready certificate.

With --json, this command group lists its available commands.

```
pb edge [flags]
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
* [pb edge list](pb_edge_list.md)	 - List hosted and self-hosted tunnel edges

