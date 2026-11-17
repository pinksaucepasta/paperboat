## pb recordings stop

Stop the recording of the terminal session you are attached to

### Synopsis

Stop recording the terminal session that a pb connect on this computer is attached to. The
session ID is optional; name it when you are attached to more than one session. A recording a
rule started can only be stopped by an admin.

JSON output is supported with --json.

```
pb recordings stop [session-id] [flags]
```

### Examples

```
  pb recordings stop
  pb recordings stop S123 --json
```

### Options

```
  -h, --help   help for stop
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

