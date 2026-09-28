## pb approve

Approve or revoke a peer device over gRPC IPC

### Synopsis

Approve or revoke a peer device over gRPC IPC

Approve an exact peer device ID for local access, or use --revoke to withdraw that approval. This changes the local daemon's device trust decision; inspect the ID before approving an unfamiliar peer.

JSON output is supported with --json.

```
pb approve <device-id> [flags]
```

### Options

```
  -h, --help     help for approve
      --json     print JSON
      --revoke   revoke peer admission
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal

