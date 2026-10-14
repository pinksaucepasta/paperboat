## pb service install

Install Paperboat daemon as the current-user service

### Synopsis

Install Paperboat daemon as the current-user service

Register the current executable as the current user's Paperboat daemon service, using the selected server and configuration. Installation does not enroll a new account by itself.

Service commands manage the current user's background Paperboat daemon through the host operating system. The service runs pb daemon; it is not a separate Paperboat binary. Use status to inspect the supervised process and restart after a local configuration change.

JSON output is supported with --json.

```
pb service install [flags]
```

### Options

```
      --config string   configuration file path
  -h, --help            help for install
      --json            print JSON
      --server string   Paperboat server URL
```

### Options inherited from parent commands

```
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb service](pb_service.md)	 - Manage the Paperboat background daemon service

