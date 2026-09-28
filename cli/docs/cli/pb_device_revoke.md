## pb device revoke

Disconnect and revoke a device

### Synopsis

Disconnect and revoke a device

Disconnect and revoke the selected enrollment. Existing device credentials lose authority; the confirmation code confirms this account-level removal.

Devices are account enrollments with independent names, capabilities, and revocation state. Changes target the selected device, while pb setup configures the current machine. Revocation disconnects an enrollment rather than just hiding it from the list.

JSON output is supported with --json.

```
pb device revoke <device> [flags]
```

### Options

```
      --confirm string   six-character confirmation code from the preview
  -h, --help             help for revoke
      --json             print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb device](pb_device.md)	 - Manage devices

