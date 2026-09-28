## pb device availability

Set device sleep availability

### Synopsis

Set device sleep availability

Set whether the selected device remains available while idle or is allowed to sleep, according to --mode. Keep-awake mode requires a confirmation code because it can affect battery use and heat.

Devices are account enrollments with independent names, capabilities, and revocation state. Changes target the selected device, while pb setup configures the current machine. Revocation disconnects an enrollment rather than just hiding it from the list.

JSON output is supported with --json.

```
pb device availability <device> [flags]
```

### Options

```
      --confirm string   six-character confirmation code for keep-awake mode
  -h, --help             help for availability
      --json             print JSON
      --mode string      availability mode: allow-sleep or keep-awake
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb device](pb_device.md)	 - Manage devices

