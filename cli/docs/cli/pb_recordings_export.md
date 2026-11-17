## pb recordings export

Write a recording as an asciicast v2 file

### Synopsis

Write one recording as an asciicast v2 file that standard players read. -o names the file;
without it the recording goes to standard output. The file is created readable by you only and
an existing file is never overwritten; a partial file is removed if the export fails. Like a
replay, the export is audited and needs your unlocked ENV vault. --archive reads the archived
copy in the team bucket instead of asking the machine.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb recordings export <recording-id> [flags]
```

### Examples

```
  pb recordings export rec_abc123 -o session.cast
  pb recordings export rec_abc123 --archive > session.cast
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

* [pb recordings](pb_recordings.md)	 - List and replay terminal recordings

