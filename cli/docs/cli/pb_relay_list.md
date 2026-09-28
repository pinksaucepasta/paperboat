## pb relay list

List hosted and self-hosted relays

### Synopsis

List relay candidates from the signed native network authority and the
signed-in account's selected self-hosted relay pool. Mixed mode shows both
sources. Self-hosted-only mode shows only selected self-hosted relays,
including unavailable ones. Sign-in and the local daemon are required.

Reported_ready means a recent signed control-plane observation, not a live
data connection. Use --json for paperboat.relay-list/v1 output with the pool
mode, relays, and observation timestamps.

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
```

### SEE ALSO

* [pb relay](pb_relay.md)	 - Inspect hosted and self-hosted relays

