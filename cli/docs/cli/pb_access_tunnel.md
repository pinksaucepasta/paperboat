## pb access tunnel

Open private TCP access to a private tunnel route

### Synopsis

Open private TCP access to an existing private tunnel route, selected by tunnel or route ID.
The command opens a loopback listener on this computer, prints its address and keeps the access
alive with periodic heartbeats; the access is released when the command ends or stops
heartbeating.

--listen must be a literal loopback address, 127.0.0.1 or ::1 with an optional port; the
default 127.0.0.1:0 picks a free port.

JSON output is supported with --json.

```
pb access tunnel <tunnel-or-route> [flags]
```

### Examples

```
  pb access tunnel myroute
  pb access tunnel tun_abc123 --listen 127.0.0.1:8080
```

### Options

```
  -h, --help            help for tunnel
      --json            print canonical JSON
      --listen string   loopback address to listen on, 127.0.0.1 or ::1 with optional port (default: a free port) (default "127.0.0.1:0")
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb access](pb_access.md)	 - Open authenticated private access

