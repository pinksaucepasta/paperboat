## pb preview inspect

Show daemon-local HTTP captures

### Synopsis

Read or manage the HTTP requests this machine's daemon captured for a preview. Filter with
--id, and page with --cursor and --limit (1-100, default 20). --enable and --disable turn
capturing on or off, --purge deletes the captured requests, and --body and --raw (bodies, exact
requests for replay) apply only together with --enable. A preview has one origin, so unlike
pb tunnel inspect there is no --route.

Captures can contain sensitive traffic such as tokens and personal data. Treat the output
accordingly.

JSON output is supported with --json.

```
pb preview inspect <preview> [flags]
```

### Examples

```
  pb preview inspect preview_abc123 --limit 10
  pb preview inspect preview_abc123 --enable --body
  pb preview inspect preview_abc123 --id cap_123 --json
```

### Options

```
      --body            capture request/response bodies with --enable
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

