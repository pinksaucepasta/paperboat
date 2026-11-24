## pb ping

Measure authenticated connectivity to a machine

### Synopsis

Send authenticated health exchanges to one machine and print, per sample, the path used
(direct, peer_relay, regional_relay or unknown) and the round-trip time, then a loss summary.
The connection is set up before the first sample and is not part of any round-trip time.
--count sets the number of samples (1-100, default 4) and --timeout bounds each (default 10s,
at most 1m). Exit status is 1 when every sample is lost. An offline machine is an error, not a
lost sample. A reachable transport does not mean the service on the machine is ready.

JSON output is supported with --json.

```
pb ping <machine> [flags]
```

### Examples

```
  pb ping studio
  pb ping studio --count 10 --timeout 5s
  pb ping studio --json
```

### Options

```
      --count int          number of round trips to measure (default 4)
  -h, --help               help for ping
      --json               print JSON
      --timeout duration   timeout for each sample, the first one including connection setup (default 10s)
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

