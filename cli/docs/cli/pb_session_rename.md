## pb session rename

Rename a terminal session

### Synopsis

Rename a terminal session

Change the display name of one durable session. This is an identity label, not a bulk rename; the default session cannot be renamed.

Terminal sessions persist independently of a single connection and can be reattached after a disconnect. Sharing grants explicit access to a named session. Closing, deleting, and removing a participant have different effects; inspect the selected session before acting.

JSON output is supported with --json.

```
pb session rename [flags]
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

* [pb session](pb_session.md)	 - Manage environment terminal sessions

