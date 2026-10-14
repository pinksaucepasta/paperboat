## pb daemon machine-guard

Manage protected local machine access

### Synopsis

Manage protected local machine access

Install or uninstall the privileged guard that protects machine-name access. The run entrypoint is invoked by operating-system supervision rather than normal interactive use.

The daemon owns this machine's background connectivity and local API. Normal lifecycle management uses pb service; daemon run is the foreground entrypoint used by service supervision. Its machine guard subcommands manage protected local machine-name access.

With --json, this command group lists its available commands.

### Options

```
  -h, --help   help for machine-guard
```

### Options inherited from parent commands

```
      --config string      configuration file
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      Paperboat server URL
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb daemon](pb_daemon.md)	 - Paperboat endpoint daemon
* [pb daemon machine-guard install](pb_daemon_machine-guard_install.md)	 - Install the root-owned machine guard service
* [pb daemon machine-guard run](pb_daemon_machine-guard_run.md)	 - Run the machine guard under service supervision
* [pb daemon machine-guard uninstall](pb_daemon_machine-guard_uninstall.md)	 - Remove machine access while retaining cached-address protection

