## pb access machine

Forward a local port to an authorized machine service

### Synopsis

Forward a loopback address on this computer to one TCP port of an enrolled machine, for
example a database that only listens on that machine. The command runs in the foreground, prints
the address it listens on, and stops on Ctrl-C (exit status 130). Only this computer can reach
the listener, up to 128 connections at once.

--port is required. --listen must be a literal loopback address, 127.0.0.1 or ::1 with an
optional port; the default 127.0.0.1:0 picks a free port. The target machine must offer the
service and its connection must be authorized. For a team machine that includes a grant of
private_access.

JSON output is supported with --json.

```
pb access machine <machine> [flags]
```

### Examples

```
  pb access machine db --port 5432
  pb access machine db --port 6379 --listen 127.0.0.1:6379
  pb access machine db --port 5432 --json
```

### Options

```
  -h, --help            help for machine
      --json            print canonical JSON
      --listen string   loopback address to listen on, 127.0.0.1 or ::1 with optional port (default: a free port) (default "127.0.0.1:0")
      --port int        TCP port on the machine (required, 1-65535)
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb access](pb_access.md)	 - Open authenticated private access

