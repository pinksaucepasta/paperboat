## pb preview

Expose a local target through a temporary preview

### Synopsis

Expose a local target through a temporary preview

JSON output is supported with --json.

```
pb preview <port|url|path> [flags]
```

### Options

```
      --background           transfer ownership to paperboatd and return after readiness
      --domain stringArray   attach a verified custom domain (repeatable)
      --duration duration    maximum preview lifetime (deprecated: use --ttl)
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
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal
* [pb preview delete](pb_preview_delete.md)	 - Delete a temporary preview
* [pb preview inspect](pb_preview_inspect.md)	 - Show daemon-local HTTP captures
* [pb preview list](pb_preview_list.md)	 - List temporary previews
* [pb preview replay](pb_preview_replay.md)	 - Deliberately replay one retained HTTP request to the same origin
* [pb preview status](pb_preview_status.md)	 - Show temporary preview status
* [pb preview stop](pb_preview_stop.md)	 - Stop a temporary preview

