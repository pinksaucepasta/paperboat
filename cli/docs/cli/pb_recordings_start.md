## pb recordings start

Start recording the terminal session you are attached to

### Synopsis

Start recording the terminal session you are attached to

Start recording the terminal session a pb connect on this computer is attached to, as your account. Everyone attached sees REC. The team decides who may: anyone with terminal access, or admins only.

Team terminal recordings are made on the recorded machine and stay there, encrypted to the team recording key. Replay is audited by Paperboat before any bytes move, then streams over the end-to-end encrypted machine connection; your unlocked vault opens the key locally.

JSON output is supported with --json.

```
pb recordings start [session-id] [flags]
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

* [pb recordings](pb_recordings.md)	 - List and replay team terminal recordings

