## pb machine add

Print Linux/macOS and Windows machine enrollment commands

### Synopsis

Print the one-shot commands that enroll a new machine: one for Linux and macOS, one for
Windows. Run the printed command on the new machine soon after: the enrollment token in it is
single-use and expires (--json shows expires_at). With --workspace the token
is for that workspace. --name suggests the machine's hostname.

If the printed commands cannot be shown, the unused enrollment is cancelled again.

JSON output is supported with --json.

```
pb machine add [flags]
```

### Examples

```
  pb machine add
  pb machine add --name db-1
  pb machine add --workspace acme --json
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

