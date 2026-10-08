## pb edge

Inspect tunnel edges

### Synopsis

Inspect tunnel edges

List account-visible tunnel edges and their observed state. The list includes hosted and selected self-hosted nodes according to the tunnel pool, with an explicit display-only hosted fallback when no self-hosted node is selected.

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

