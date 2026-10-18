## pb approve

Approve a peer machine over gRPC IPC

### Synopsis

Approve a peer machine over gRPC IPC

Approve an exact peer machine ID for local access. Inspect the ID before approving an unfamiliar peer. Withdrawing a machine is `pb machine revoke`, which asks for confirmation; pb approve never revokes.

JSON output is supported with --json.

```
pb approve <machine-id> [flags]
```

### Options

```
  -h, --help   help for approve
      --json   print JSON
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

