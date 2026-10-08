## pb daemon machine-guard uninstall

Remove machine access while retaining cached-address protection

### Synopsis

Remove machine access while retaining cached-address protection

Remove the privileged machine-access integration while retaining cached-address protection. This does not uninstall the ordinary current-user Paperboat daemon service.

The machine guard is a privileged local helper for protected loopback listeners and trusted local HTTPS. Installation handles certificate trust; public DNS supplies the browser names. Use the dedicated service commands for the ordinary Paperboat daemon.

JSON output is supported with --json.

```
pb daemon machine-guard uninstall [flags]
```

### Options

```
  -h, --help   help for uninstall
      --json   print JSON
```

### Options inherited from parent commands

```
      --config string      configuration file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      Paperboat server URL
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb daemon machine-guard](pb_daemon_machine-guard.md)	 - Manage protected local machine access

