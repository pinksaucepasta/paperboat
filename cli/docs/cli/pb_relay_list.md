## pb relay list

List hosted and self-hosted relays

### Synopsis

List the relay candidates this computer can use: the relays from the signed network
authority and the self-hosted relays your account has selected. In mixed pool mode both appear.
In self-hosted-only mode only the selected self-hosted relays appear, including unavailable
ones, and Paperboat's relays are never used instead.

STATUS is reported_ready when a signed control-plane observation of the relay is under 15
seconds old; it is not a live connection test. LATENCY is the connect time this machine last
measured to the relay, as its daemon reported it (measured about every 15 minutes, shown while
under an hour old). It is "-" until a measurement exists. The numbers are for display and never
choose a path.

Sign-in and the local daemon are required. With --json the output is paperboat.relay-list/v1:
the pool mode and the relays, each with latency_ms and latency_measured_at once measured.

JSON output is supported with --json.

```
pb relay list [flags]
```

### Examples

```
  pb relay list
  pb relay list --json
```

### Options

```
  -h, --help   help for list
      --json   print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb relay](pb_relay.md)	 - Inspect hosted and self-hosted relays

