## pb port name set

Name a machine port

### Synopsis

Give a port a name, or move an existing name to another port. With --machine the
address is <name>.<machine>.<domain>; with --all-machines the name works on every machine
of the active workspace; with --top-level --machine the address is <name>.<domain>.

JSON output is supported with --json.

```
pb port name set <name> <port> [flags]
```

### Examples

```
  pb port name set app 3000 --machine homelab
  pb port name set grafana 3000 --all-machines
  pb port name set jellyfin 8096 --top-level --machine media
```

### Options

```
      --all-machines     name this port on every machine of the workspace
  -h, --help             help for set
      --json             print JSON
      --machine string   the machine whose port is named
      --top-level        make <name>.<domain> open the port of --machine
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb port name](pb_port_name.md)	 - Name machine ports for browser addresses

