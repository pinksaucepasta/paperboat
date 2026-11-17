## pb send status

Inspect a file transfer

### Synopsis

Show one outgoing transfer: its delivery progress and receipt. --on is required: name the
machine the files were sent to. Delivered to the receiver is not the same as verified by it;
only the receiver's verified receipt means done.

JSON output is supported with --json.

```
pb send status <transfer-id> [flags]
```

### Examples

```
  pb send status transfer_abc --on laptop
  pb send status transfer_abc --on laptop --json
```

### Options

```
  -h, --help             help for status
      --json             print JSON
      --on string        machine the files were sent to, by name or ID (required)
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

* [pb send](pb_send.md)	 - Send files or folders to a machine's Paperboat Inbox

