## pb preview replay

Deliberately replay one retained HTTP request to the same origin

### Synopsis

Send one captured HTTP request again to the preview's origin, by capture ID (from
pb preview inspect). This repeats whatever the request did, including side effects. Pass
--idempotency-key when the origin supports one, to retry safely. A preview has one origin, so
unlike pb tunnel replay there is no --route.

JSON output is supported with --json.

```
pb preview replay <preview> <capture-id> [flags]
```

### Examples

```
  pb preview replay preview_abc123 cap_456
  pb preview replay preview_abc123 cap_456 --idempotency-key order-42
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
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb preview](pb_preview.md)	 - Expose a local target through a temporary preview

