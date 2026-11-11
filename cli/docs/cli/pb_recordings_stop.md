## pb recordings stop

Stop the recording of the terminal session you are attached to

### Synopsis

Stop the recording of the terminal session you are attached to

Stop the recording of the terminal session a pb connect on this computer is attached to. You may stop a recording you started; a recording a team rule started stops only for an admin.

Team terminal recordings are made on the recorded machine and stay there, encrypted to the team recording key. Replay is audited by Paperboat before any bytes move, then streams over the end-to-end encrypted machine connection; your unlocked vault opens the key locally.

JSON output is supported with --json.

```
pb recordings stop [session-id] [flags]
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

* [pb recordings](pb_recordings.md)	 - List and replay team terminal recordings

