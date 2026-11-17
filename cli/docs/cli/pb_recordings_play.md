## pb recordings play

Replay a recording in this terminal

### Synopsis

Replay one recording in this terminal at its recorded pace. --speed scales the pace and
--idle-limit caps any pause between frames (default 2s); both must be positive. The replay
is audited and needs your ENV vault unlocked; if it is locked the command says to run
pb env unlock. --archive replays the archived copy in the team bucket instead of asking the
machine, which is useful when the machine is offline.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb recordings play <recording-id> [flags]
```

### Examples

```
  pb recordings play rec_abc123
  pb recordings play rec_abc123 --speed 2 --idle-limit 1s
  pb recordings play rec_abc123 --archive
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

* [pb recordings](pb_recordings.md)	 - List and replay terminal recordings

