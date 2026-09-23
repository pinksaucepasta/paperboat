## pb machine capabilities

Set incoming services for a device

### Synopsis

Set incoming services for a device

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
      --peer-relay       relay encrypted traffic for your devices
      --preview-tunnel   serve preview and tunnel routes
      --terminal         accept Paperboat terminal and exec
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb machine](pb_machine.md)	 - Manage machines

