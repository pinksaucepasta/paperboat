## pb inbox policy

Show or update team file acceptance

### Synopsis

Show or update team file acceptance

Show or set manual versus automatic acceptance for team file requests. --receipt-email controls receipt notification behavior, not file retention.

The Paperboat Inbox receives verified files and exact team requests. Files remain until removed by the user. The receiving path is a local device setting; team request acceptance is an account policy. A sender manages outgoing batches with pb send.

JSON output is supported with --json.

```
pb inbox policy [manual|automatic] [flags]
```

### Options

```
  -h, --help            help for policy
      --json            
      --receipt-email   
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb inbox](pb_inbox.md)	 - Manage the Paperboat Inbox

