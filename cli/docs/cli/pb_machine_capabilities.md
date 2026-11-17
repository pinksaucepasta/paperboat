## pb machine capabilities

Set incoming services for a machine

### Synopsis

Choose which incoming services a machine accepts. Each flag is true or false; at least one
is required and the others stay as they are:

  --terminal        Paperboat terminal and pb exec
  --managed-ssh     managed SSH, SCP, SFTP and rsync
  --file-receive    files sent to its Inbox with pb send
  --preview-tunnel  serving previews and tunnels

A disabled service stays unavailable even while the machine is online. The command saves the
setting and reports how far it has been applied.

JSON output is supported with --json.

```
pb machine capabilities <machine> [flags]
```

### Examples

```
  pb machine capabilities db --terminal=true --managed-ssh=true
  pb machine capabilities db --file-receive=false
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

