## pb connect

Create and attach to an environment terminal session

### Synopsis

Open a durable terminal session on a machine (an "environment") and attach this terminal to
it. Without --session it always creates a fresh session, optionally named with --name; a
session keeps running when you disconnect. Attach to an existing one with --session (its name
or ID), or with pb session attach. --name and --session cannot be combined. A second argument
new is accepted and means the same as leaving --session out; it cannot be combined with
--session.

The status bar shows connection and recording state: --status-bar auto, on or off,
--status-bar-fullscreen hide or show, --status-bar-theme terminal, dark, light or mono.
--debug shows the pb versions in use. If the connection drops, pb reconnects; at the start of a
line, Enter ~ r reconnects now, Enter ~ . disconnects, and ~~ sends a literal ~.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb connect <environment> [new] [flags]
```

### Examples

```
  pb connect homelab
  pb connect homelab --name deploy
  pb connect homelab --session deploy
  pb connect homelab --status-bar off
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

