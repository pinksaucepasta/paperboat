## pb device rename

Rename a device

### Synopsis

Rename a device

Change the display name of one enrolled device. The enrollment identity and grants stay attached to the device; callers should use the returned name for future selection.

Devices are account enrollments with independent names, capabilities, and revocation state. Changes target the selected device, while pb setup configures the current machine. Revocation disconnects an enrollment rather than just hiding it from the list.

JSON output is supported with --json.

```
pb device rename <device> <name> [flags]
```

### Options

```
  -h, --help   help for rename
      --json   print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb device](pb_device.md)	 - Manage devices

