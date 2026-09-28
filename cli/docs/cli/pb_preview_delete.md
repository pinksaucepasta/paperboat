## pb preview delete

Delete a temporary preview

### Synopsis

Delete a temporary preview

End the selected temporary preview and its published access. This command currently performs the same stop operation as pb preview stop; use status first if you need its current state.

A preview exposes a local target through a temporary Paperboat URL or private/team access policy. Inspect its status before sharing the URL, and stop or delete it when no longer needed. Browser HTTP terminates TLS at the edge.

JSON output is supported with --json.

```
pb preview delete <preview> [flags]
```

### Options

```
  -h, --help   help for delete
      --json   print the deleted canonical preview resource as JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb preview](pb_preview.md)	 - Expose a local target through a temporary preview

