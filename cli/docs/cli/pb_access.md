## pb access

Open authenticated private access

### Synopsis

Open authenticated private access

Choose machine access for an enrolled machine service or tunnel access for an existing private tunnel route. Each child command opens a local listener and keeps the forwarding process alive until it exits.

With --json, this command group lists its available commands.

### Options

```
  -h, --help   help for access
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
* [pb access machine](pb_access_machine.md)	 - Forward a local port to an authorized machine service
* [pb access tunnel](pb_access_tunnel.md)	 - Open private TCP access through stable hostd

