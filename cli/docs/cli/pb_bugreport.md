## pb bugreport

Create a redacted Paperboat diagnostic bundle

### Synopsis

Create a redacted Paperboat diagnostic bundle

Collect a bounded, redacted diagnostic bundle for support. --record controls capture and --upload sends the prepared report; review the output path and support reference before sharing it.

JSON output is supported with --json.

```
pb bugreport [flags]
```

### Options

```
  -h, --help     help for bugreport
      --json     print JSON
      --record   record reproduction start and end markers
      --upload   upload the exact redacted bundle
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal

