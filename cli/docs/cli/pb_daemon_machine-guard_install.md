## pb daemon machine-guard install

Install the root-owned machine guard service

### Synopsis

Install the root-owned machine guard service

Install the root-owned guard service for the fixed protected 127.100.0.0/16 loopback range and configure local HTTPS certificate trust automatically.

The machine guard is a privileged local helper for protected loopback listeners and trusted local HTTPS. Installation handles certificate trust; public DNS supplies the browser names. Use the dedicated service commands for the ordinary Paperboat daemon.

JSON output is supported with --json.

```
pb daemon machine-guard install [flags]
```

### Options

```
  -h, --help   help for install
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

