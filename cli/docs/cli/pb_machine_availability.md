## pb machine availability

Set machine sleep availability

### Synopsis

Set machine sleep availability

Set whether the selected machine remains available while idle or is allowed to sleep, according to --mode. Keep-awake mode requires a confirmation code because it can affect battery use and heat.

Machines are account enrollments with independent names, capabilities, and revocation state. Changes target the selected machine, while pb setup configures the current machine. Revocation disconnects an enrollment rather than just hiding it from the list.

JSON output is supported with --json.

```
pb machine availability <machine> [flags]
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
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb machine](pb_machine.md)	 - Manage machines

