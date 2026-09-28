## pb inbox decline

Decline an exact team file request

### Synopsis

Decline an exact team file request

Reject one exact pending team file request at its expected generation. The sender receives a decline rather than an implicit timeout.

The Paperboat Inbox receives verified files and exact team requests. Files remain until removed by the user. The receiving path is a local device setting; team request acceptance is an account policy. A sender manages outgoing batches with pb send.

JSON output is supported with --json.

```
pb inbox decline <request-id> [flags]
```

### Options

```
      --generation uint   
  -h, --help              help for decline
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

