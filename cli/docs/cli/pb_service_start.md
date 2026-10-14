## pb service start

Start Paperboat daemon current-user service

### Synopsis

Start Paperboat daemon current-user service

Start the installed current-user daemon service. A successful supervisor action should be followed by status or doctor when endpoint readiness matters.

Service commands manage the current user's background Paperboat daemon through the host operating system. The service runs pb daemon; it is not a separate Paperboat binary. Use status to inspect the supervised process and restart after a local configuration change.

JSON output is supported with --json.

```
pb service start [flags]
```

### Options

```
  -h, --help   help for start
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

