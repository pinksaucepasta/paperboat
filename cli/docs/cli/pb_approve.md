## pb approve

Approve or revoke a peer machine over gRPC IPC

### Synopsis

Approve or revoke a peer machine over gRPC IPC

Approve an exact peer machine ID for local access, or use --revoke to withdraw that approval. This changes the local daemon's machine trust decision; inspect the ID before approving an unfamiliar peer.

JSON output is supported with --json.

```
pb approve <machine-id> [flags]
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
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal

