## pb send destination

Show the default transfer destination

### Synopsis

Show the saved default machine for pb send, or with --session the destination saved for one
terminal session. --to on a single send overrides it. This setting only chooses where your
sends go; it does not change any Inbox directory.

JSON output is supported with --json.

```
pb send destination [flags]
```

### Examples

```
  pb send destination
  pb send destination --session S123 --json
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
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb send](pb_send.md)	 - Send files or folders to a machine's Paperboat Inbox
* [pb send destination clear](pb_send_destination_clear.md)	 - Clear the default transfer destination
* [pb send destination set](pb_send_destination_set.md)	 - Set the default transfer destination

