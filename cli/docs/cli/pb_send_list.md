## pb send list

List file transfers

### Synopsis

List outgoing transfers to one machine. --on is required: name the machine the
files were sent to. Filter with --q (file name or transfer ID), --state and --session; page
with --limit (default 50) and --offset. Use a listed transfer ID with status or cancel. For
sends to this computer the history is read from the local Inbox.

JSON output is supported with --json.

```
pb send list [flags]
```

### Examples

```
  pb send list --on laptop
  pb send list --on laptop --state failed --json
```

### Options

```
  -h, --help             help for list
      --json             print JSON
      --limit int        maximum transfers to return (default 50)
      --offset int       continue at a transfer history offset
      --on string        machine the files were sent to, by name or ID (required)
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

* [pb send](pb_send.md)	 - Send files or folders to a machine's Paperboat Inbox

