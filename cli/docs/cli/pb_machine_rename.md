## pb machine rename

Rename a machine

### Synopsis

Rename a machine

Change the display name of one enrolled machine. The enrollment identity and grants stay attached to the machine; callers should use the returned name for future selection.

Machines are account enrollments with independent names, capabilities, and revocation state. Changes target the selected machine, while pb setup configures the current machine. Revocation disconnects an enrollment rather than just hiding it from the list.

JSON output is supported with --json.

```
pb machine rename <machine> <name> [flags]
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
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb machine](pb_machine.md)	 - Manage machines

