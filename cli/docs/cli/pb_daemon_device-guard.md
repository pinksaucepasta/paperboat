## pb daemon device-guard

Manage protected device-name access

### Synopsis

Manage protected device-name access

Install or uninstall the privileged guard that protects device-name access. The run entrypoint is invoked by operating-system supervision rather than normal interactive use.

The daemon owns this device's background connectivity and local API. Normal lifecycle management uses pb service; daemon run is the foreground entrypoint used by service supervision. Its device guard subcommands manage protected local device-name access.

With --json, this command group lists its available commands.

### Options

```
  -h, --help   help for device-guard
```

### Options inherited from parent commands

```
      --config string      configuration file
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      Paperboat server URL
```

### SEE ALSO

* [pb daemon](pb_daemon.md)	 - Paperboat endpoint daemon
* [pb daemon device-guard install](pb_daemon_device-guard_install.md)	 - Install the root-owned device guard service
* [pb daemon device-guard run](pb_daemon_device-guard_run.md)	 - Run the device guard under service supervision
* [pb daemon device-guard uninstall](pb_daemon_device-guard_uninstall.md)	 - Remove device access while retaining cached-address protection

