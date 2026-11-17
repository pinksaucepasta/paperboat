## pb recordings start

Start recording the terminal session you are attached to

### Synopsis

Start recording the terminal session that a pb connect on this computer is attached to.
Everyone attached sees a REC mark while it records. The session ID is optional; name it when
you are attached to more than one session.

Only admins start recordings (on a personal machine, its owner). The machine needs a recording
key (set up in the console under Terminals > Recordings) and a team also needs the Teams plan;
a refusal says which condition is missing.

JSON output is supported with --json.

```
pb recordings start [session-id] [flags]
```

### Examples

```
  pb recordings start
  pb recordings start S123 --json
```

### Options

```
  -h, --help   help for start
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

* [pb recordings](pb_recordings.md)	 - List and replay terminal recordings

