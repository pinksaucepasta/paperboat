## pb send destination set

Set the default transfer destination

### Synopsis

Set the default transfer destination

Save one enrolled machine as the default recipient for future sends. A command's explicit --to selection takes precedence over this local default.

Send publishes a batch to a machine's Paperboat Inbox. Completion means the receiver verified and recorded the files; status and cancel act on a transfer ID. A default destination can be set separately, while --to chooses one for the current send.

JSON output is supported with --json.

```
pb send destination set <machine> [flags]
```

### Options

```
  -h, --help   help for set
      --json   print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --session string     terminal session ID for a session-specific destination
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb send destination](pb_send_destination.md)	 - Show the default transfer destination

