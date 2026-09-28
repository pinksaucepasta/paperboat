## pb preview replay

Deliberately replay one retained HTTP request to the same origin

### Synopsis

Deliberately replay one retained HTTP request to the same origin

Replay one retained HTTP request to the same preview origin by capture ID. This deliberately repeats an HTTP action; use an idempotency key when the origin supports one.

A preview exposes a local target through a temporary Paperboat URL or private/team access policy. Inspect its status before sharing the URL, and stop or delete it when no longer needed. Browser HTTP terminates TLS at the edge.

JSON output is supported with --json.

```
pb preview replay <preview> <capture-id> [flags]
```

### Options

```
  -h, --help                     help for replay
      --idempotency-key string   explicit idempotency key for safe retry (generated when omitted)
      --json                     print canonical JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb preview](pb_preview.md)	 - Expose a local target through a temporary preview

