## pb ping

Measure authenticated connectivity to a machine

### Synopsis

Measure authenticated connectivity to a machine

JSON output is supported with --json.

```
pb ping <machine> [flags]
```

### Options

```
      --count int          number of authenticated health exchanges (default 4)
  -h, --help               help for ping
      --json               print JSON
      --timeout duration   timeout for each exchange (default 10s)
      --transport string   peer transport: a, d, q, w, or r (default "a")
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal

