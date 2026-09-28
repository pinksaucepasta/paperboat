## pb send destination

Show the default transfer destination

### Synopsis

Show the default transfer destination

Show the saved default device for outgoing sends. This setting does not affect the Inbox receiving directory and can be overridden for one send with --to.

Send publishes a batch to a device's Paperboat Inbox. Completion means the receiver verified and recorded the files; status and cancel act on a transfer ID. A default destination can be set separately, while --to chooses one for the current send.

JSON output is supported with --json.

```
pb send destination [flags]
```

### Options

```
  -h, --help             help for destination
      --json             print JSON
      --session string   terminal session ID for a session-specific destination
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb send](pb_send.md)	 - Send files to a device's Paperboat Inbox
* [pb send destination clear](pb_send_destination_clear.md)	 - Clear the default transfer destination
* [pb send destination set](pb_send_destination_set.md)	 - Set the default transfer destination

