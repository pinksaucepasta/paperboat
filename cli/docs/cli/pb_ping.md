## pb ping

Measure authenticated connectivity to a machine

### Synopsis

Measure authenticated connectivity to a machine

Send authenticated health exchanges to one enrolled device. --count and --timeout bound the measurements; a reachable transport alone is not a claim that the target service is ready.

JSON output is supported with --json.

```
pb ping <machine> [flags]
```

### Options

```
      --count int          number of authenticated native connections (default 4)
  -h, --help               help for ping
      --json               print JSON
      --timeout duration   timeout for each connection (default 10s)
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal

