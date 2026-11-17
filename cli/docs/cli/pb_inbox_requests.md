## pb inbox requests

List team file requests

### Synopsis

List team file requests, one per line: request ID, sender account, number of files,
status and expiry. A request is a sender asking to deliver an exact set of files; approve or
decline it by its request ID. Filter with --owner, --q and --state; the list stops at --limit
unless you pass --all.

JSON output is supported with --json.

```
pb inbox requests [flags]
```

### Examples

```
  pb inbox requests
  pb inbox requests --state pending --json
```

### Options

```
      --all            show every item, up to 10000
  -h, --help           help for requests
      --json           print JSON
      --limit int      show at most this many items (1-200) (default 50)
      --owner string   filter owner: mine, shared, or an authorized account ID
      --q string       filter by resource name or ID
      --state string   filter by resource state
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb inbox](pb_inbox.md)	 - Manage the Paperboat Inbox

