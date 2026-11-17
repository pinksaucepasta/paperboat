## pb relay

Inspect hosted and self-hosted relays

### Synopsis

Inspect the relays this computer can use. A relay carries traffic between machines when
they cannot reach each other directly. The list combines the relays Paperboat runs with the
self-hosted relays your account has selected, and shows how long this machine took to reach
each one.

With --json, this command group lists its available commands.

```
pb relay [flags]
```

### Examples

```
  pb relay list
  pb relay list --json
```

### Options

```
  -h, --help   help for relay
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal
* [pb relay list](pb_relay_list.md)	 - List hosted and self-hosted relays

