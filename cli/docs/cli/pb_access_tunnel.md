## pb access tunnel

Open private TCP access through stable hostd

### Synopsis

Open private TCP access through stable hostd

Resolve a tunnel or route by its exact identity and forward a loopback listener to that authorized private TCP target. --listen selects the local address; the listener is closed when the command ends.

Private access requires authorization for the exact target. The listener stays on the local loopback interface, and closing this command closes the forwarding session. Public tunnel publication is managed separately with pb tunnel.

JSON output is supported with --json.

```
pb access tunnel <tunnel-or-route> [flags]
```

### Options

```
  -h, --help            help for tunnel
      --json            print canonical JSON
      --listen string   literal-loopback listen address (default "127.0.0.1:0")
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

