## pb device capabilities

Set incoming services for a device

### Synopsis

Set incoming services for a device

Enable or disable incoming terminal, managed SSH, file receive, preview tunnel, and peer relay services for one device. Disabled services are not made available simply because the device is online.

Devices are account enrollments with independent names, capabilities, and revocation state. Changes target the selected device, while pb setup configures the current machine. Revocation disconnects an enrollment rather than just hiding it from the list.

JSON output is supported with --json.

```
pb device capabilities <device> [flags]
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

* [pb device](pb_device.md)	 - Manage devices

