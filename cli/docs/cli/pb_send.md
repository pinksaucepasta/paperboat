## pb send

Send files or folders to a machine's Paperboat Inbox

### Synopsis

Send files or folders to a machine's Paperboat Inbox. Name the machine with --to. Without
--to the command uses the saved default (pb send destination set) or, inside a terminal
session, that session's destination; it refuses to guess when more than one machine fits.

A folder arrives as a folder of the same name. Links and special files are not sent and are
listed as skipped. The destination must be enrolled, not revoked, and accept incoming files
(pb machine capabilities --file-receive). Sending to this computer copies into its own Inbox.

On a team that approves incoming files, the command waits for the recipient's decision and
exits with an error if it is declined. The command succeeds only once the receiver has verified
and recorded the files. --unlimited lifts the size limit by sending only directly between the
two machines or through your own relay, and waits while neither is available; without it your
account's transfer mode applies.

JSON output is supported with --json.

```
pb send <path>... --to <machine> [flags]
```

### Examples

```
  pb send report.pdf --to db
  pb send ./dist --to laptop
  pb send notes.txt
  pb send big.iso --to db --unlimited
```

### Options

```
  -h, --help             help for send
      --json             print JSON
      --session string   terminal session ID for destination context
      --to string        destination machine name or ID
      --unlimited        send any size, only directly or through your own relay; waits while neither is available (default: your account's transfer mode)
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

