## pb session

Manage environment terminal sessions

### Synopsis

Manage environment terminal sessions

List, attach, rename, close, or delete durable terminal sessions, and manage exact session sharing. Use --all only on commands that expose it and inspect the affected environment first.

With --json, this command group lists its available commands.

```
pb session [flags]
```

### Options

```
  -h, --help   help for session
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal
* [pb session attach](pb_session_attach.md)	 - Choose and attach to a durable terminal session
* [pb session close](pb_session_close.md)	 - Close one or all terminal sessions
* [pb session delete](pb_session_delete.md)	 - Delete a closed terminal session record
* [pb session join](pb_session_join.md)	 - Join an explicitly shared terminal with recent output
* [pb session list](pb_session_list.md)	 - List durable terminal sessions
* [pb session participants](pb_session_participants.md)	 - Show terminal sharing and connected participants
* [pb session remove](pb_session_remove.md)	 - Remove a teammate, including access through an all-team grant
* [pb session rename](pb_session_rename.md)	 - Rename a terminal session
* [pb session share](pb_session_share.md)	 - Grant a team or teammate viewer or interactive access
* [pb session shared](pb_session_shared.md)	 - List owned and explicitly shared terminal sessions
* [pb session unshare](pb_session_unshare.md)	 - End sharing while preserving the owner's terminal

