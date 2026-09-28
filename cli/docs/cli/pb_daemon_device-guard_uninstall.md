## pb daemon device-guard uninstall

Remove device access while retaining cached-address protection

### Synopsis

Remove device access while retaining cached-address protection

Remove the privileged device-access integration while retaining cached-address protection. This does not uninstall the ordinary current-user Paperboat daemon service.

The device guard is a privileged local helper for protected device-name resolution. Installation and removal change local operating-system integration; use the dedicated service commands for the ordinary Paperboat daemon.

JSON output is supported with --json.

```
pb daemon device-guard uninstall [flags]
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
```

### SEE ALSO

* [pb daemon device-guard](pb_daemon_device-guard.md)	 - Manage protected device-name access

