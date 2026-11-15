## pb port name

Name machine ports for browser addresses

### Synopsis

Give a machine port a name: <name>.<machine>.<domain> then opens that port in a
browser on every computer of everyone allowed that port. A name never grants access.
--machine names one machine's port (machine managers). --all-machines names that port
on every machine of the active workspace (owners and admins); a machine name of the
same label wins. --top-level --machine makes <name>.<domain> open that machine's port
(owners and admins); a machine with that name wins, and pb port name list shows the
top-level name as shadowed. Numeric labels always mean the port.

With --json, this command group lists its available commands.

```
pb port name [flags]
```

### Options

```
  -h, --help   help for name
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

* [pb port](pb_port.md)	 - Name machine ports and set a machine's front desk
* [pb port name list](pb_port_name_list.md)	 - List the workspace's port names
* [pb port name set](pb_port_name_set.md)	 - Name a machine port
* [pb port name unset](pb_port_name_unset.md)	 - Remove a port name

