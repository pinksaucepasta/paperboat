## pb daemon

Paperboat endpoint daemon

### Synopsis

Paperboat endpoint daemon

Run the daemon under supervision or manage the privileged device guard. Most users should use pb service to install, start, stop, and inspect the background process.

With --json, this command group lists its available commands.

```
pb daemon [flags]
```

### Options

```
      --config string   configuration file
  -h, --help            help for daemon
      --server string   Paperboat server URL
```

### Options inherited from parent commands

```
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal
* [pb daemon device-guard](pb_daemon_device-guard.md)	 - Manage protected device-name access
* [pb daemon run](pb_daemon_run.md)	 - Run Paperboat daemon under service supervision

