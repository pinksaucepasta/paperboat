## pb session unshare

End sharing while preserving the owner's terminal

### Synopsis

End sharing while preserving the owner's terminal

End sharing for one session while preserving the owner's terminal and history. Existing participants lose the shared grant according to the resulting authorization state.

Terminal sessions persist independently of a single connection and can be reattached after a disconnect. Sharing grants explicit access to a named session. Closing, deleting, and removing a participant have different effects; inspect the selected session before acting.

JSON output is supported with --json.

```
pb session unshare <session-id> [flags]
```

### Options

```
  -h, --help   help for unshare
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

