## pb inbox set



### Synopsis

Set an explicit local directory for future received files. The command configures the receiving side; it does not move already delivered files.

The Paperboat Inbox receives verified files and exact team requests. Files remain until removed by the user. The receiving path is a local machine setting; team request acceptance is an account policy. A sender manages outgoing batches with pb send.

JSON output is supported with --json.

```
pb inbox set <directory> [flags]
```

### Options

```
  -h, --help   help for set
      --json   
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb inbox](pb_inbox.md)	 - Manage the Paperboat Inbox

