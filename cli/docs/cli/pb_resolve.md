## pb resolve

Resolve a peer machine IP, port forwardings over gRPC IPC

### Synopsis

Resolve a peer machine IP, port forwardings over gRPC IPC

Ask the local daemon to resolve a peer machine query into authorized address, forwarding, and tag information. The IP is the address this computer's ports for that machine listen on: connect raw TCP (a database, SSH, a game server) to that IP and port. Machine names resolve to the local web gateway and reach HTTP, HTTPS and WebSocket only. This reports locally known peer state; it does not approve a new machine.

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
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal

