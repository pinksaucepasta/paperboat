## pb access device

Forward a local port to an authorized device service

### Synopsis

Forward a loopback TCP port in this process's network namespace to one authorized device service. The forward runs in the foreground; processes sharing the namespace can connect to it.

JSON output is supported with --json.

```
pb access device <machine> [flags]
```

### Options

```
  -h, --help            help for device
      --json            print canonical JSON
      --listen string   literal-loopback listen address (default "127.0.0.1:0")
      --port int        remote device TCP port
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb access](pb_access.md)	 - Open authenticated private access

