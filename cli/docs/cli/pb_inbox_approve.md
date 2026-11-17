## pb inbox approve

Approve an exact team file request

### Synopsis

Approve one pending team file request. The sender can then deliver exactly the files in the
request, no others. File names are checked against this computer's rules first, and a request
this system cannot store is refused before it is approved.

--generation is optional: without it the command uses the request's current generation. Pass it
to approve only if the request has not changed since you looked at it.

JSON output is supported with --json.

```
pb inbox approve <request-id> [flags]
```

### Examples

```
  pb inbox requests
  pb inbox approve request_abc123
```

### Options

```
      --generation uint   expected request generation (default: the request's current one)
  -h, --help              help for approve
      --json              print JSON
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

