## pb tag

Assign tags to a machine over gRPC IPC

### Synopsis

Assign tags to a machine over gRPC IPC

Assign the supplied tags to an exact machine ID through the local daemon. Tags aid machine discovery and resolution; they do not grant access by themselves.

JSON output is supported with --json.

```
pb tag <machine-id> <tag1> [tag2...] [flags]
```

### Options

```
  -h, --help   help for tag
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

