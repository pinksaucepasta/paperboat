## pb access

Open authenticated private access

### Synopsis

Open private access to something that is never published: a service on one of your
enrolled machines (machine) or an existing private tunnel route (tunnel). Each command opens a
listener on a loopback address of this computer and runs in the foreground; Ctrl-C closes it
and the access ends with it. Nothing here makes a public URL; public access is pb tunnel and
pb preview. Access is authorized for the exact machine, port or route, and team machines also
need the matching grant.

With --json, this command group lists its available commands.

```
pb access [flags]
```

### Examples

```
  pb access machine db --port 5432
  pb access tunnel myroute --listen 127.0.0.1:8080
```

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
* [pb access tunnel](pb_access_tunnel.md)	 - Open private TCP access to a private tunnel route

