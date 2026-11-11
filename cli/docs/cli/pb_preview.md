## pb preview

Expose a local target through a temporary preview

### Synopsis

Expose a local target through a temporary preview

Start a temporary preview for a local port, URL, or path. --private or --team controls who may open it; public browser HTTP ends TLS at the edge. Use status and stop to manage its lifecycle.

JSON output is supported with --json.

```
pb preview <port|url|path> [flags]
```

### Options

```
      --background           transfer ownership to paperboatd and return after readiness
      --domain stringArray   attach a verified custom domain (repeatable)
  -h, --help                 help for preview
      --json                 print the canonical preview resource as JSON
      --private              limit the preview to this account
      --team                 require an explicit team grant for browser access
      --ttl duration         bounded preview lifetime (maximum 24h)
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal
* [pb preview delete](pb_preview_delete.md)	 - Delete a temporary preview
* [pb preview inspect](pb_preview_inspect.md)	 - Show daemon-local HTTP captures
* [pb preview ip-rules](pb_preview_ip-rules.md)	 - Show or set which client addresses may open this preview
* [pb preview list](pb_preview_list.md)	 - List temporary previews
* [pb preview replay](pb_preview_replay.md)	 - Deliberately replay one retained HTTP request to the same origin
* [pb preview status](pb_preview_status.md)	 - Show temporary preview status
* [pb preview stop](pb_preview_stop.md)	 - Stop a temporary preview

