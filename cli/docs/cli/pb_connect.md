## pb connect

Create and attach to an environment terminal session

### Synopsis

Create and attach to an environment terminal session

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
      --transport string               peer transport: a (auto), d (direct QUIC), q (relay QUIC), w (relay WSS), or r (relay race)
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal

