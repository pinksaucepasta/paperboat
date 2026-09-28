## pb send status

Inspect a file transfer

### Synopsis

Inspect a file transfer

Inspect one outgoing batch's delivery progress and receipt by transfer ID. --on selects the device when needed; successful publication is distinct from final receiver verification.

Send publishes a batch to a device's Paperboat Inbox. Completion means the receiver verified and recorded the files; status and cancel act on a transfer ID. A default destination can be set separately, while --to chooses one for the current send.

JSON output is supported with --json.

```
pb send status <transfer-id> [flags]
```

### Options

```
  -h, --help             help for status
      --json             print JSON
      --on string        destination machine name or ID
      --session string   terminal session ID for destination context
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb send](pb_send.md)	 - Send files to a device's Paperboat Inbox

