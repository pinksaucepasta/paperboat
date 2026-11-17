## pb wait

Wait for a machine readiness condition

### Synopsis

Wait until a machine satisfies a readiness condition chosen with --for. runtime waits for an
eligible runtime that is ready or degraded. transport (the default) also probes an authenticated
connection to the machine. ssh also waits for managed SSH to be ready. --timeout bounds the wait
(up to 24h); mere connectivity is never treated as readiness.

Exit status: 0 ready, 203 timed out, 205 cancelled, 1 any other failure.

JSON output is supported with --json.

```
pb wait <machine> [flags]
```

### Examples

```
  pb wait studio
  pb wait studio --for ssh --timeout 10m
  pb wait studio --json
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
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal

