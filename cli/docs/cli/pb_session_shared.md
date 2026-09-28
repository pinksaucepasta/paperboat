## pb session shared

List owned and explicitly shared terminal sessions

### Synopsis

List owned and explicitly shared terminal sessions

List sessions you own and sessions explicitly shared with you. This inventory shows access, not a blanket right to all sessions in a team environment.

Terminal sessions persist independently of a single connection and can be reattached after a disconnect. Sharing grants explicit access to a named session. Closing, deleting, and removing a participant have different effects; inspect the selected session before acting.

JSON output is supported with --json.

```
pb session shared [flags]
```

### Options

```
  -h, --help   help for shared
      --json   print canonical JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb session](pb_session.md)	 - Manage environment terminal sessions

