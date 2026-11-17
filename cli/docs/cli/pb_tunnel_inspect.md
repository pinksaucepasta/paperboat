## pb tunnel inspect

Show daemon-local HTTP captures

### Synopsis

Read or manage the HTTP requests this machine's daemon captured for a tunnel. Name the
route with --route when the tunnel has several; filter with --id, and page with --cursor and
--limit (1-100, default 20). --enable and --disable turn capturing on or off, --purge deletes
the captured requests, and --body and --raw (bodies, exact requests for replay) apply only
together with --enable.

Captures can contain sensitive traffic such as tokens and personal data. Treat the output
accordingly.

JSON output is supported with --json.

```
pb tunnel inspect <tunnel> [flags]
```

### Examples

```
  pb tunnel inspect web --limit 10
  pb tunnel inspect web --route route_xyz --id cap_123
  pb tunnel inspect web --enable --body
  pb tunnel inspect web --purge
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
      --route string    inspect one route ID (required when the tunnel has several routes)
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --ephemeral          use the temporary preview lifecycle
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb tunnel](pb_tunnel.md)	 - Manage durable tunnels or start an ephemeral tunnel

