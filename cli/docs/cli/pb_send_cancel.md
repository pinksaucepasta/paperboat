## pb send cancel

Cancel a file transfer batch

### Synopsis

Cancel a file transfer batch

Cancel an outgoing transfer by its exact transfer ID, optionally selecting a machine with --on. Cancellation reports the resulting batch state; already delivered files are not silently deleted.

Send publishes a batch to a machine's Paperboat Inbox. Completion means the receiver verified and recorded the files; status and cancel act on a transfer ID. A default destination can be set separately, while --to chooses one for the current send.

JSON output is supported with --json.

```
pb send cancel <transfer-id> [flags]
```

### Options

```
  -h, --help             help for cancel
      --json             print JSON
      --on string        destination machine name or ID
      --session string   terminal session ID for destination context
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

