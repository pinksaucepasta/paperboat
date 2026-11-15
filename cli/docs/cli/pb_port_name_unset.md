## pb port name unset

Remove a port name

### Synopsis

Remove one port name of the active workspace. Say which one with --machine (a name
of that machine's port), --all-machines or --top-level. The same people who may set a
name may remove it; the port itself and its access do not change.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb port name unset <name> [flags]
```

### Examples

```
  pb port name unset app --machine homelab
  pb port name unset grafana --all-machines
  pb port name unset jellyfin --top-level
```

### Options

```
      --all-machines     name this port on every machine of the workspace
  -h, --help             help for unset
      --machine string   the machine whose port is named
      --top-level        make <name>.<domain> open the port of --machine
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

* [pb port name](pb_port_name.md)	 - Name machine ports for browser addresses

