## pb inbox decline

Decline an exact team file request

### Synopsis

Decline one pending team file request. The sender is told it was declined and cannot
deliver those files. --generation is optional: without it the command uses the request's
current generation.

JSON output is supported with --json.

```
pb inbox decline <request-id> [flags]
```

### Examples

```
  pb inbox decline request_abc123
  pb inbox decline request_abc123 --json
```

### Options

```
      --generation uint   expected request generation (default: the request's current one)
  -h, --help              help for decline
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

