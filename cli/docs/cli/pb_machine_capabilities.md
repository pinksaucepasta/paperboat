## pb machine capabilities

Set incoming services for a machine

### Synopsis

Set incoming services for a machine

Enable or disable incoming terminal, managed SSH, file receive, and preview tunnel services for one machine. Disabled services are not made available simply because the machine is online.

Machines are account enrollments with independent names, capabilities, and revocation state. Changes target the selected machine, while pb setup configures the current machine. Revocation disconnects an enrollment rather than just hiding it from the list.

JSON output is supported with --json.

```
pb machine capabilities <machine> [flags]
```

### Options

```
      --file-receive     accept native Inbox transfers
  -h, --help             help for capabilities
      --json             print JSON
      --managed-ssh      accept managed SSH/SCP/SFTP/rsync
      --preview-tunnel   serve preview and tunnel routes
      --terminal         accept Paperboat terminal and exec
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb machine](pb_machine.md)	 - Manage machines

