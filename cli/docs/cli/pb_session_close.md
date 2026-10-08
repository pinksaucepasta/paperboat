## pb session close

Close one or all terminal sessions

### Synopsis

Close one or all terminal sessions

Close one selected terminal session or every applicable session with --all. Closing ends the remote process and deletes its recent output; the closed record remains temporarily. With --all, run the command once to receive a short-lived confirmation token, then run the displayed command to proceed. The same code flow applies to one session.

Terminal sessions persist independently of a single connection and can be reattached after a disconnect. Sharing grants explicit access to a named session. Closing, deleting, and removing a participant have different effects; inspect the selected session before acting.

JSON output is supported with --json.

```
pb session close [flags]
```

### Options

```
      --all              close all sessions in the environment
      --confirm string   six-character confirmation code from the preview
  -h, --help             help for close
      --json             print JSON
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

