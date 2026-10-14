## pb machine add

Print Linux/macOS and Windows machine enrollment commands

### Synopsis

Print Linux/macOS and Windows machine enrollment commands

Create an enrollment command for Linux, macOS, or Windows and optionally give the new machine a name. The printed one-shot material should be used on the intended machine.

Machines are account enrollments with independent names, capabilities, and revocation state. Changes target the selected machine, while pb setup configures the current machine. Revocation disconnects an enrollment rather than just hiding it from the list.

JSON output is supported with --json.

```
pb machine add [flags]
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
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb machine](pb_machine.md)	 - Manage machines

