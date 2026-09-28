## pb resolve

Resolve a peer device IP, port forwardings and tags over gRPC IPC

### Synopsis

Resolve a peer device IP, port forwardings and tags over gRPC IPC

Ask the local daemon to resolve a peer device query into authorized address, forwarding, and tag information. This reports locally known peer state; it does not approve a new device.

JSON output is supported with --json.

```
pb resolve <query> [flags]
```

### Options

```
  -h, --help   help for resolve
      --json   output JSON format
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal

