## pb preview stop

Stop a temporary preview

### Synopsis

Stop a temporary preview

Stop the selected temporary preview's active forwarding. Other previews and durable tunnels remain separate resources.

A preview exposes a local target through a temporary Paperboat URL or private/team access policy. Inspect its status before sharing the URL, and stop or delete it when no longer needed. Browser HTTP terminates TLS at the edge.

JSON output is supported with --json.

```
pb preview stop <preview> [flags]
```

### Options

```
  -h, --help   help for stop
      --json   print the stopped canonical preview resource as JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb preview](pb_preview.md)	 - Expose a local target through a temporary preview

