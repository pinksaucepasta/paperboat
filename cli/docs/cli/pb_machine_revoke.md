## pb machine revoke

Disconnect and revoke a machine

### Synopsis

Disconnect and revoke a machine

Disconnect and revoke the selected enrollment. Existing machine credentials lose authority; the confirmation code confirms this account-level removal.

Machines are account enrollments with independent names, capabilities, and revocation state. Changes target the selected machine, while pb setup configures the current machine. Revocation disconnects an enrollment rather than just hiding it from the list.

JSON output is supported with --json.

```
pb machine revoke <machine> [flags]
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
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb machine](pb_machine.md)	 - Manage machines

