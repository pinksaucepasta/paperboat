## pb preview status

Show temporary preview status

### Synopsis

Show temporary preview status

Show the selected preview's current publication and connector state. A URL existing in the account is distinct from a ready, reachable origin.

A preview exposes a local target through a temporary Paperboat URL or private/team access policy. Inspect its status before sharing the URL, and stop or delete it when no longer needed. Browser HTTP terminates TLS at the edge.

JSON output is supported with --json.

```
pb preview status <preview> [flags]
```

### Options

```
  -h, --help   help for status
      --json   print the canonical preview resource as JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb preview](pb_preview.md)	 - Expose a local target through a temporary preview

