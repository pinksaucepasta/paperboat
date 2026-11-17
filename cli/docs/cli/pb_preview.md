## pb preview

Expose a local target through a temporary preview

### Synopsis

Expose a local target through a temporary preview URL. The target is a bare port (3000
means http://127.0.0.1:3000), an http, https, h2c or tcp URL with an explicit port and nothing
else (no path, query or user), or an absolute Unix socket path (also written unix:///path).

A preview is public by default. --private limits it to your account and --team requires an
explicit team grant (they cannot be combined). --ttl limits its lifetime to at most 24h.
--host-header sends a different Host to the origin, for servers that allow-list one (http, https
and h2c targets; give a bare name without a port). --domain attaches a verified custom domain
and waits up to 15 minutes for its DNS and certificate.

The preview runs while the command runs; Ctrl+C stops it. --background hands it to this
computer's background service and returns once it is ready, with a default lifetime of 30
minutes.

JSON output is supported with --json.

```
pb preview <port|url|path> [flags]
```

### Examples

```
  pb preview 3000
  pb preview https://127.0.0.1:8443 --private --ttl 2h
  pb preview unix:///run/app.sock --host-header myapp.test
  pb preview 3000 --background --json
```

### Options

```
      --background           hand the preview to this computer's background service and return once ready (default lifetime 30m)
      --domain stringArray   attach a verified custom domain (repeatable)
  -h, --help                 help for preview
      --host-header string   send this Host to the origin instead of the public host (http, https, h2c targets; for dev servers with a host allow-list)
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

