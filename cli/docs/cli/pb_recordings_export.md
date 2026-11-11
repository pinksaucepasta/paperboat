## pb recordings export

Write a recording as an asciicast v2 file

### Synopsis

Write a recording as an asciicast v2 file

Write one recording as an asciicast v2 file that standard players read. The machine sends it encrypted and your unlocked vault decrypts it here; Paperboat records the export as a replay first.

Team terminal recordings are made on the recorded machine and stay there, encrypted to the team recording key. Replay is audited by Paperboat before any bytes move, then streams over the end-to-end encrypted machine connection; your unlocked vault opens the key locally.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb recordings export <recording-id> [flags]
```

### Options

```
      --archive         read the copy in the team bucket instead of asking the machine
  -h, --help            help for export
  -o, --output string   file to write; standard output when omitted
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

