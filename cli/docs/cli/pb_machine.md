## pb machine

Manage machines

### Synopsis

Manage machines

List account machines, print a one-shot enrollment command, and manage one machine's name, availability, capabilities, or revocation. All mutations are scoped to an exact enrollment; a machine's online state does not by itself grant a service capability.

With --json, this command group lists its available commands.

```
pb machine [flags]
```

### Options

```
  -h, --help   help for machine
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
* [pb machine add](pb_machine_add.md)	 - Print Linux/macOS and Windows machine enrollment commands
* [pb machine availability](pb_machine_availability.md)	 - Set machine sleep availability
* [pb machine capabilities](pb_machine_capabilities.md)	 - Set incoming services for a machine
* [pb machine list](pb_machine_list.md)	 - List enrolled machines
* [pb machine rename](pb_machine_rename.md)	 - Rename a machine
* [pb machine revoke](pb_machine_revoke.md)	 - Disconnect and revoke a machine

