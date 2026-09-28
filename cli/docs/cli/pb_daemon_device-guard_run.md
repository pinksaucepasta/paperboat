## pb daemon device-guard run

Run the device guard under service supervision

### Synopsis

Run the device guard under service supervision

Start the foreground device-guard process for its supervisor. This entrypoint is used by the installed root-owned service and normally should not be run from an interactive shell.

The device guard is a privileged local helper for protected device-name resolution. Installation and removal change local operating-system integration; use the dedicated service commands for the ordinary Paperboat daemon.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb daemon device-guard run [flags]
```

### Options

```
  -h, --help   help for run
```

### Options inherited from parent commands

```
      --config string      configuration file
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      Paperboat server URL
```

### SEE ALSO

* [pb daemon device-guard](pb_daemon_device-guard.md)	 - Manage protected device-name access

