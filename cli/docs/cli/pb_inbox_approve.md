## pb inbox approve

Approve an exact team file request

### Synopsis

Approve an exact team file request

Approve one exact pending team file request at its expected generation. The approved sender can deliver only the requested batch; review request details before accepting.

The Paperboat Inbox receives verified files and exact team requests. Files remain until removed by the user. The receiving path is a local device setting; team request acceptance is an account policy. A sender manages outgoing batches with pb send.

JSON output is supported with --json.

```
pb inbox approve <request-id> [flags]
```

### Options

```
      --generation uint   
  -h, --help              help for approve
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

