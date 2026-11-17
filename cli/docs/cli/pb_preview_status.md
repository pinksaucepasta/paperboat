## pb preview status

Show temporary preview status

### Synopsis

Show one preview's access, target, state and deadline. A preview that exists is not
necessarily reachable: if nothing answers on the target yet, visitors see "App not responding"
until your app starts.

JSON output is supported with --json.

```
pb preview status <preview> [flags]
```

### Examples

```
  pb preview status preview_abc123
  pb preview status preview_abc123 --json
```

### Options

```
  -h, --help   help for status
      --json   print the canonical preview resource as JSON
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

