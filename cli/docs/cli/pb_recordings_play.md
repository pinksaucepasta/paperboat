## pb recordings play

Replay a recording in this terminal

### Synopsis

Replay a recording in this terminal

Replay one recording in this terminal at its recorded pace, with long pauses shortened by --idle-limit. Paperboat records the replay before the machine sends the encrypted recording; your unlocked vault decrypts it here.

Team terminal recordings are made on the recorded machine and stay there, encrypted to the team recording key. Replay is audited by Paperboat before any bytes move, then streams over the end-to-end encrypted machine connection; your unlocked vault opens the key locally.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb recordings play <recording-id> [flags]
```

### Options

```
      --archive               replay the copy in the team bucket instead of asking the machine
  -h, --help                  help for play
      --idle-limit duration   longest pause between frames (default 2s)
      --speed float           playback speed (default 1)
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

* [pb recordings](pb_recordings.md)	 - List and replay team terminal recordings

