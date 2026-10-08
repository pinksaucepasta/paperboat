## pb preview inspect

Show daemon-local HTTP captures

### Synopsis

Show daemon-local HTTP captures

Read or manage bounded HTTP captures retained by the local daemon for one preview. --body and --raw expose capture content, while enable, disable, and purge change capture state; treat output as potentially sensitive.

A preview exposes a local target through a temporary Paperboat URL or private/team access policy. Inspect its status before sharing the URL, and stop or delete it when no longer needed. Browser HTTP terminates TLS at the edge.

JSON output is supported with --json.

```
pb preview inspect <preview> [flags]
```

### Options

```
      --body            capture redacted request/response bodies with --enable
      --cursor string   continue after a capture cursor
      --disable         disable capture for this resource
      --enable          enable capture for this resource
  -h, --help            help for inspect
      --id string       show one capture by ID
      --json            print canonical JSON
      --limit int       maximum captures per request (1-100) (default 20)
      --purge           purge retained captures for this resource
      --raw             retain exact raw requests for replay with --enable
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb preview](pb_preview.md)	 - Expose a local target through a temporary preview

