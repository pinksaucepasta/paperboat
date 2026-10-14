## pb access machine

Forward a local port to an authorized machine service

### Synopsis

Forward a loopback TCP port in this process's network namespace to one authorized machine service. The forward runs in the foreground; processes sharing the namespace can connect to it.

--port identifies the exact service on the authorized machine, and --listen chooses the local loopback address. Closing the command closes the forwarding session without publishing a public URL.

Private access requires authorization for the exact target. The listener stays on the local loopback interface, and closing this command closes the forwarding session. Public tunnel publication is managed separately with pb tunnel.

JSON output is supported with --json.

```
pb access machine <machine> [flags]
```

### Options

```
  -h, --help            help for machine
      --json            print canonical JSON
      --listen string   literal-loopback listen address (default "127.0.0.1:0")
      --port int        remote machine TCP port
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

