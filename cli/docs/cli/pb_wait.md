## pb wait

Wait for a machine readiness condition

### Synopsis

Wait for a machine readiness condition

JSON output is supported with --json.

```
pb wait <machine> [flags]
```

### Options

```
      --for string         readiness condition: runtime, transport, or ssh (default "transport")
  -h, --help               help for wait
      --json               print JSON
      --timeout duration   maximum time to wait (default 5m0s)
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal

