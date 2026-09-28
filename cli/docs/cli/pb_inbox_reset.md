## pb inbox reset



### Synopsis

Restore the default local receiving directory for future files. The account's team request acceptance policy and already delivered files are unchanged.

The Paperboat Inbox receives verified files and exact team requests. Files remain until removed by the user. The receiving path is a local device setting; team request acceptance is an account policy. A sender manages outgoing batches with pb send.

JSON output is supported with --json.

```
pb inbox reset [flags]
```

### Options

```
  -h, --help   help for reset
      --json   
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb inbox](pb_inbox.md)	 - Manage the Paperboat Inbox

