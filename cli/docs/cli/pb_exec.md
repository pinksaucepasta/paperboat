## pb exec

Execute an exact command on a machine

### Synopsis

Execute an exact command on a machine

JSON output is supported with --json.

```
pb exec <machine> [flags] -- <argv...>
```

### Options

```
      --cwd string         absolute remote working directory
      --env stringArray    remote environment name=value
  -h, --help               help for exec
      --json               emit paperboat.exec-event/v1 JSON Lines
      --pty                allocate a remote PTY
      --timeout duration   remote execution timeout
      --transport string   peer transport: a, d, q, w, or r
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal

