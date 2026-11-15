## pb port

Name machine ports and set a machine's front desk

### Synopsis

Name machine ports for private browser addresses and choose where unnamed addresses of a
machine go. Names live on the server and work on every computer of everyone allowed
the port; they never grant access to a port.

With --json, this command group lists its available commands.

```
pb port [flags]
```

### Options

```
  -h, --help   help for port
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
* [pb port front-desk](pb_port_front-desk.md)	 - Show or set where unnamed addresses of a machine go
* [pb port name](pb_port_name.md)	 - Name machine ports for browser addresses

