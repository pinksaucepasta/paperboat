## pb session attach

Choose and attach to a durable terminal session

### Synopsis

Choose and attach to a durable terminal session

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb session attach [environment] [session] [flags]
```

### Options

```
      --debug                          show the pb versions used by this terminal session
  -h, --help                           help for attach
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
```

### SEE ALSO

* [pb session](pb_session.md)	 - Manage environment terminal sessions

