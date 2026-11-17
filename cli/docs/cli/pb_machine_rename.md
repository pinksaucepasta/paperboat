## pb machine rename

Rename a machine

### Synopsis

Rename a machine, selected by its current name or ID. The new name must follow the machine
name rules; a refused name is explained without echoing what you typed. Its ID and grants
stay as they are. Afterwards the local machine list refreshes, so the new name resolves at once.

JSON output is supported with --json.

```
pb machine rename <machine> <name> [flags]
```

### Examples

```
  pb machine rename old-name db-1
  pb machine rename m_abc123 db-1 --json
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

