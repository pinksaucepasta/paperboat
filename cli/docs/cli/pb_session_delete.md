## pb session delete

Delete a closed terminal session record

### Synopsis

Delete a closed terminal session record

Delete a closed non-default terminal session record, or use --all for all such records in the environment. Close a running terminal first. With --all, run the command once to receive a short-lived confirmation token, then run the displayed command to proceed. the confirmation code confirms only single-session deletion.

Terminal sessions persist independently of a single connection and can be reattached after a disconnect. Sharing grants explicit access to a named session. Closing, deleting, and removing a participant have different effects; inspect the selected session before acting.

JSON output is supported with --json.

```
pb session delete [flags]
```

### Options

```
      --all              delete all sessions in the environment
      --confirm string   six-character confirmation code from the preview
  -h, --help             help for delete
      --json             print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb session](pb_session.md)	 - Manage environment terminal sessions

