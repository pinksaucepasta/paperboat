## pb inbox

Manage the Paperboat Inbox

### Synopsis

Manage the Paperboat Inbox

Inspect or set the local receiving directory, the account's team request acceptance policy, and pending team file requests. Approve or decline an exact request by ID; outgoing transfers are sent and managed under pb send.

With --json, this command group lists its available commands.

```
pb inbox [flags]
```

### Options

```
  -h, --help   help for inbox
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
* [pb inbox approve](pb_inbox_approve.md)	 - Approve an exact team file request
* [pb inbox decline](pb_inbox_decline.md)	 - Decline an exact team file request
* [pb inbox path](pb_inbox_path.md)	 - 
* [pb inbox policy](pb_inbox_policy.md)	 - Show or update team file acceptance
* [pb inbox requests](pb_inbox_requests.md)	 - List team file requests
* [pb inbox reset](pb_inbox_reset.md)	 - 
* [pb inbox set](pb_inbox_set.md)	 - 

