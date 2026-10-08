## pb connect

Create and attach to an environment terminal session

### Synopsis

Create and attach to an environment terminal session

Select an environment by name, create or reattach a durable terminal session, and bridge the local terminal. Use --session for a specific session or new to start another one; connection status is reported separately from readiness.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb connect <environment> [new] [flags]
```

### Options

```
      --debug                          show the pb versions used by this terminal session
  -h, --help                           help for connect
      --name string                    name for the fresh terminal session
      --session string                 attach an existing terminal session by name or ID
      --status-bar string              status bar for this attach: auto, on, or off
      --status-bar-fullscreen string   status bar in full-screen applications: hide or show
      --status-bar-theme string        status bar theme: terminal, dark, light, or mono
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal

