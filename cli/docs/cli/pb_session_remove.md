## pb session remove

Remove a teammate, including access through an all-team grant

### Synopsis

Remove a teammate, including access through an all-team grant

Remove one teammate from a session, including access that came through an all-team grant. The owner's session remains active for other authorized participants.

Terminal sessions persist independently of a single connection and can be reattached after a disconnect. Sharing grants explicit access to a named session. Closing, deleting, and removing a participant have different effects; inspect the selected session before acting.

JSON output is supported with --json.

```
pb session remove <session-id> <account-id> [flags]
```

### Options

```
  -h, --help   help for remove
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

