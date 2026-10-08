## pb send

Send files to a machine's Paperboat Inbox

### Synopsis

Send files to a machine's Paperboat Inbox

Send one or more paths to the selected machine's Inbox. The command succeeds only after receiver verification and a durable receipt; --session associates the transfer with a terminal session.

JSON output is supported with --json.

```
pb send <path>... --to <machine> [flags]
```

### Options

```
  -h, --help             help for send
      --json             print JSON
      --session string   terminal session ID for destination context
      --to string        destination machine name or ID
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal
* [pb send cancel](pb_send_cancel.md)	 - Cancel a file transfer batch
* [pb send destination](pb_send_destination.md)	 - Show the default transfer destination
* [pb send list](pb_send_list.md)	 - List file transfers
* [pb send status](pb_send_status.md)	 - Inspect a file transfer

