## pb bugreport

Create a redacted Paperboat diagnostic bundle

### Synopsis

Collect a bounded, redacted diagnostic bundle for support. The bundle holds a manifest,
recent events, redacted events and status. Nothing leaves this machine unless you pass --upload;
--record controls capture. The command prints where the bundle is and its support reference;
review it before you share it.

JSON output is supported with --json.

```
pb bugreport [flags]
```

### Examples

```
  pb bugreport
  pb bugreport --upload
  pb bugreport --json
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

