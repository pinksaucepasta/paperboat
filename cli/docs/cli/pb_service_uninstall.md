## pb service uninstall

Uninstall Paperboat daemon current-user service

### Synopsis

Uninstall Paperboat daemon current-user service

Remove the current-user daemon service registration. This does not by itself revoke the account's device enrollment or uninstall the privileged device guard.

Service commands manage the current user's background Paperboat daemon through the host operating system. The service runs pb daemon; it is not a separate Paperboat binary. Use status to inspect the supervised process and restart after a local configuration change.

JSON output is supported with --json.

```
pb service uninstall [flags]
```

### Options

```
  -h, --help   help for uninstall
      --json   print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb service](pb_service.md)	 - Manage the Paperboat background daemon service

