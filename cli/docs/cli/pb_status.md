## pb status

Show local Paperboat machine status

### Synopsis

Show local Paperboat machine status

Show the local endpoint's Paperboat state or the selected device's observed status. This is a snapshot rather than a readiness wait; use doctor for a deeper check, service status for supervisor state, and pb wait for a named readiness condition.

JSON output is supported with --json.

```
pb status [machine] [flags]
```

### Options

```
  -h, --help   help for status
      --json   print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal

