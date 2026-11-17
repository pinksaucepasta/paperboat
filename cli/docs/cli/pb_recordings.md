## pb recordings

List and replay terminal recordings

### Synopsis

List, replay and export terminal recordings, and start or stop recording a session you are
attached to. Team recordings are made on the recorded machine and stay there, encrypted for the
team; the team's bucket can hold an archived copy. Replay and export are audited by Paperboat
before any data moves, travel over the end-to-end encrypted machine connection, and are
decrypted on your computer with your unlocked ENV vault (pb env unlock).

With --json, this command group lists its available commands.

```
pb recordings [flags]
```

### Examples

```
  pb recordings list
  pb recordings play rec_abc123 --speed 2
  pb recordings export rec_abc123 -o session.cast
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

