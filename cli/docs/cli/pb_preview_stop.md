## pb preview stop

Stop a temporary preview

### Synopsis

Stop one temporary preview, selected by its ID or name, and withdraw its published access. Other
previews and durable tunnels are not affected. The command prints the stopped preview, or with
--json the stopped resource. pb preview delete does the same.

JSON output is supported with --json.

```
pb preview stop <preview> [flags]
```

### Examples

```
  pb preview stop preview_abc123
  pb preview stop preview_abc123 --json
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
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb preview](pb_preview.md)	 - Expose a local target through a temporary preview

