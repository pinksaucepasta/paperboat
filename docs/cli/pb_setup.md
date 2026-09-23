## pb setup

Set up this machine for Paperboat

### Synopsis

Set up this machine for Paperboat

JSON output is supported with --json.

```
pb setup [flags]
```

### Options

```
      --file-receive             accept native Inbox transfers (default true)
  -h, --help                     help for setup
      --json                     print JSON
      --managed-ssh              accept managed SSH tools (default true)
      --name string              machine name
      --peer-relay               relay encrypted traffic for your devices
      --preview-tunnel           serve previews and tunnels (default true)
      --recovery-output string   new absolute file for the account recovery key
      --ssh-port uint            existing loopback sshd port (default 22)
      --state-root string        runtime state directory
      --terminal                 accept Paperboat terminal and exec (default true)
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal

