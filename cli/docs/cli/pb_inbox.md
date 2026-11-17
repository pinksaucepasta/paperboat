## pb inbox

Manage the Paperboat Inbox

### Synopsis

Manage this machine's Paperboat Inbox, where files sent to it arrive. Three settings and one
queue live here: the local directory received files are written to (path, set, reset), the
account's rule for team file requests (policy), and the team file requests waiting for your
decision (requests, approve, decline). Files stay in the Inbox until you remove them. Sending
is pb send.

With --json, this command group lists its available commands.

```
pb inbox [flags]
```

### Examples

```
  pb inbox path
  pb inbox requests
  pb inbox policy automatic
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
* [pb inbox path](pb_inbox_path.md)	 - Show this machine's Inbox directory
* [pb inbox policy](pb_inbox_policy.md)	 - Show or update team file acceptance
* [pb inbox requests](pb_inbox_requests.md)	 - List team file requests
* [pb inbox reset](pb_inbox_reset.md)	 - Restore the default Inbox directory
* [pb inbox set](pb_inbox_set.md)	 - Set this machine's Inbox directory

