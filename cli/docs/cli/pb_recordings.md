## pb recordings

List and replay team terminal recordings

### Synopsis

List and replay team terminal recordings

List the team terminal recordings you may see, start or stop recording a session you are attached to, and replay or export one from the machine that recorded it. Each replay is audited first; the recording key opens with your unlocked vault on this computer.

With --json, this command group lists its available commands.

```
pb recordings [flags]
```

### Options

```
  -h, --help   help for recordings
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
* [pb recordings export](pb_recordings_export.md)	 - Write a recording as an asciicast v2 file
* [pb recordings list](pb_recordings_list.md)	 - List the recordings you may see
* [pb recordings play](pb_recordings_play.md)	 - Replay a recording in this terminal
* [pb recordings start](pb_recordings_start.md)	 - Start recording the terminal session you are attached to
* [pb recordings stop](pb_recordings_stop.md)	 - Stop the recording of the terminal session you are attached to

