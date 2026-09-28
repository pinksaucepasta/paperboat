## pb daemon device-guard install

Install the root-owned device guard service

### Synopsis

Install the root-owned device guard service

Install the root-owned guard service and its protected loopback range. --loopback-cidr selects the permitted local range; review it before changing host integration.

The device guard is a privileged local helper for protected device-name resolution. Installation and removal change local operating-system integration; use the dedicated service commands for the ordinary Paperboat daemon.

JSON output is supported with --json.

```
pb daemon device-guard install [flags]
```

### Options

```
  -h, --help                   help for install
      --json                   print JSON
      --loopback-cidr string   validated local device loopback /16
```

### Options inherited from parent commands

```
      --config string      configuration file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      Paperboat server URL
```

### SEE ALSO

* [pb daemon device-guard](pb_daemon_device-guard.md)	 - Manage protected device-name access

