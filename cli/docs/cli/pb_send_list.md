## pb send list

List file transfers

### Synopsis

List file transfers

List outgoing transfer batches, optionally limited by machine, terminal session, or result count. Use a returned transfer ID with status or cancel.

Send publishes a batch to a machine's Paperboat Inbox. Completion means the receiver verified and recorded the files; status and cancel act on a transfer ID. A default destination can be set separately, while --to chooses one for the current send.

JSON output is supported with --json.

```
pb send list [flags]
```

### Options

```
  -h, --help             help for list
      --json             print JSON
      --limit int        maximum transfers to return (default 50)
      --offset int       continue at a transfer history offset
      --on string        destination machine name or ID
      --q string         filter by file basename or transfer ID
      --session string   terminal session ID for destination context
      --state string     filter by transfer state
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb send](pb_send.md)	 - Send files to a machine's Paperboat Inbox

