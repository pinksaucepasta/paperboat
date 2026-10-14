## pb session join

Join an explicitly shared terminal with recent output

### Synopsis

Join an explicitly shared terminal with recent output

Join an explicitly shared terminal by session ID. Your viewer or interactive role comes from the owner's grant; joining does not create a new ownership session.

Terminal sessions persist independently of a single connection and can be reattached after a disconnect. Sharing grants explicit access to a named session. Closing, deleting, and removing a participant have different effects; inspect the selected session before acting.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb session join <session-id> [flags]
```

### Options

```
  -h, --help   help for join
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb session](pb_session.md)	 - Manage environment terminal sessions

