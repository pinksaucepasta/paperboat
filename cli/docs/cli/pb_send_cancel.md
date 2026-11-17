## pb send cancel

Cancel a file transfer batch

### Synopsis

Cancel one outgoing transfer batch by its transfer ID. --on is required: name the machine
the files were sent to. The command prints the batch's resulting state; files that were already
delivered are not deleted.

JSON output is supported with --json.

```
pb send cancel <transfer-id> [flags]
```

### Examples

```
  pb send cancel transfer_abc --on laptop
```

### Options

```
  -h, --help             help for cancel
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

