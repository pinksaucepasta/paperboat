## pb inbox requests

List team file requests

### Synopsis

List team file requests

List pending team file requests and their identities for review. Use approve or decline with the exact request ID and current generation.

The Paperboat Inbox receives verified files and exact team requests. Files remain until removed by the user. The receiving path is a local device setting; team request acceptance is an account policy. A sender manages outgoing batches with pb send.

JSON output is supported with --json.

```
pb inbox requests [flags]
```

### Options

```
  -h, --help   help for requests
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

