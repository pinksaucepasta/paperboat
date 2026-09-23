## pb preview inspect

Show daemon-local HTTP captures

### Synopsis

Show daemon-local HTTP captures

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
```

### SEE ALSO

* [pb preview](pb_preview.md)	 - Expose a local target through a temporary preview

