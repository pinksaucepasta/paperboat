## pb session list

List durable terminal sessions

### Synopsis

List durable terminal sessions

List durable sessions for one environment or the current selection. --wide adds more detail; choose an exact session ID for attach, rename, close, or sharing.

Terminal sessions persist independently of a single connection and can be reattached after a disconnect. Sharing grants explicit access to a named session. Closing, deleting, and removing a participant have different effects; inspect the selected session before acting.

JSON output is supported with --json.

```
pb session list [environment] [flags]
```

### Options

```
  -h, --help   help for list
      --json   print JSON
      --wide   include immutable IDs
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb session](pb_session.md)	 - Manage environment terminal sessions

