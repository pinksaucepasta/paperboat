## pb recordings list

List the recordings you may see

### Synopsis

List the recordings you may see

List a team's terminal recordings you may see: admins see all of them, other members those they may replay. Each row shows the recording ID, machine, start time, length, why it started and whether the machine still holds it.

Team terminal recordings are made on the recorded machine and stay there, encrypted to the team recording key. Replay is audited by Paperboat before any bytes move, then streams over the end-to-end encrypted machine connection; your unlocked vault opens the key locally.

JSON output is supported with --json.

```
pb recordings list [flags]
```

### Options

```
      --all              show every item, up to 10000
  -h, --help             help for list
      --json             print JSON
      --limit int        show at most this many items (1-200) (default 50)
      --machine string   only this machine's recordings (machine id)
      --team string      team whose recordings to list; defaults to the active team workspace
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

