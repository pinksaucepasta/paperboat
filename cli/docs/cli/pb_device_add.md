## pb device add

Print Linux/macOS and Windows device enrollment commands

### Synopsis

Print Linux/macOS and Windows device enrollment commands

Create an enrollment command for Linux, macOS, or Windows and optionally give the new device a name. The printed one-shot material should be used on the intended machine.

Devices are account enrollments with independent names, capabilities, and revocation state. Changes target the selected device, while pb setup configures the current machine. Revocation disconnects an enrollment rather than just hiding it from the list.

JSON output is supported with --json.

```
pb device add [flags]
```

### Options

```
  -h, --help          help for add
      --json          print JSON
      --name string   optional machine hostname
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb device](pb_device.md)	 - Manage devices

