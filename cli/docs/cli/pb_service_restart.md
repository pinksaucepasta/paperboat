## pb service restart

Restart Paperboat daemon current-user service

### Synopsis

Restart Paperboat daemon current-user service

Restart the supervised current-user daemon after a local configuration or binary change. Active connections may be interrupted; inspect pb service status afterward.

Service commands manage the current user's background Paperboat daemon through the host operating system. The service runs pb daemon; it is not a separate Paperboat binary. Use status to inspect the supervised process and restart after a local configuration change.

JSON output is supported with --json.

```
pb service restart [flags]
```

### Options

```
  -h, --help   help for restart
      --json   print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb service](pb_service.md)	 - Manage the Paperboat background daemon service

