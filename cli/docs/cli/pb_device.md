## pb device

Manage devices

### Synopsis

Manage devices

List account devices, print a one-shot enrollment command, and manage one device's name, availability, capabilities, or revocation. All mutations are scoped to an exact enrollment; a device's online state does not by itself grant a service capability.

With --json, this command group lists its available commands.

```
pb device [flags]
```

### Options

```
  -h, --help   help for device
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal
* [pb device add](pb_device_add.md)	 - Print Linux/macOS and Windows device enrollment commands
* [pb device availability](pb_device_availability.md)	 - Set device sleep availability
* [pb device capabilities](pb_device_capabilities.md)	 - Set incoming services for a device
* [pb device list](pb_device_list.md)	 - List enrolled devices
* [pb device rename](pb_device_rename.md)	 - Rename a device
* [pb device revoke](pb_device_revoke.md)	 - Disconnect and revoke a device

