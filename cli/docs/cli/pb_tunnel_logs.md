## pb tunnel logs

Show tunnel logs

### Synopsis

Show recent events of a tunnel, 1 to 200 per page (--limit, default 100; --cursor continues).
--follow waits for new entries every --interval (250ms to 1m, default 1s). Logs explain what
happened; for the current state use pb tunnel status.

JSON output is supported with --json.

```
pb tunnel logs <tunnel> [flags]
```

### Examples

```
  pb tunnel logs web
  pb tunnel logs web --follow --interval 5s
  pb tunnel logs web --limit 50 --json
```

### Options

```
      --cursor string       continue after a log cursor
      --follow              wait for new log entries
  -h, --help                help for logs
      --interval duration   follow polling interval (250ms-1m) (default 1s)
      --json                print canonical JSON
      --limit int           maximum entries per request (1-200) (default 100)
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

