## pb preview list

List temporary previews

### Synopsis

List temporary previews

List account-visible temporary previews and their state. Select an exact preview identity for status, inspect, replay, stop, or delete.

A preview exposes a local target through a temporary Paperboat URL or private/team access policy. Inspect its status before sharing the URL, and stop or delete it when no longer needed. Browser HTTP terminates TLS at the edge.

JSON output is supported with --json.

```
pb preview list [flags]
```

### Options

```
  -h, --help   help for list
      --json   print canonical preview resources as JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb preview](pb_preview.md)	 - Expose a local target through a temporary preview

