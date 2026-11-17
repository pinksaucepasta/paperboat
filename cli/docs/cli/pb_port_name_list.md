## pb port name list

List the workspace's port names

### Synopsis

List the port names of the active workspace: names of its machines' ports, its
all-machines names and its top-level names. A top-level name whose label is also a
machine's name is shown as shadowed: that address opens the machine.

JSON output is supported with --json.

```
pb port name list [flags]
```

### Examples

```
  pb port name list
  pb port name list --json
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

* [pb port name](pb_port_name.md)	 - Name machine ports for browser addresses

