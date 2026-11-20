## pb edge

Inspect the regions tunnels and previews are served from

### Synopsis

Inspect the regions that serve the active workspace's tunnels and previews: Paperboat's
regions, or the regions of the self-hosted servers you selected. Listing a region never grants
a route admission.

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
* [pb edge list](pb_edge_list.md)	 - List the regions tunnels and previews are served from

