## pb preview replay

Deliberately replay one retained HTTP request to the same origin

### Synopsis

Deliberately replay one retained HTTP request to the same origin

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

