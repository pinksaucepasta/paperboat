## pb machine

Manage machines

### Synopsis

List and manage the machines enrolled in your account: print the command that enrolls a new
one (add), rename it, choose whether it may sleep (availability), choose which incoming services
it accepts (capabilities), tag it, or revoke it. These commands name any enrolled machine by name
or ID; pb setup is what configures the machine you are sitting at. Revoking disconnects the
enrollment, it does not just hide the machine from the list.

In an interactive terminal, pb machine with no subcommand opens the machine picker. Elsewhere it
prints this help.

With --json, this command group lists its available commands.

```
pb machine [flags]
```

### Examples

```
  pb machine list
  pb machine add --name db-1
  pb machine revoke old-laptop
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
* [pb machine tag](pb_machine_tag.md)	 - Show or change a machine's tags

