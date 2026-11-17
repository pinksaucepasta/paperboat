## pb preview delete

Delete a temporary preview

### Synopsis

Delete a temporary preview

End the selected temporary preview and its published access. This command currently performs the same stop operation as pb preview stop; use status first if you need its current state.

JSON output is supported with --json.

```
pb preview delete <preview> [flags]
```

### Examples

```
  pb preview delete preview_abc123
```

### Options

```
  -h, --help   help for delete
      --json   print the deleted canonical preview resource as JSON
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

