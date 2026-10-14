## pb daemon

Paperboat endpoint daemon

### Synopsis

Paperboat endpoint daemon

Run the daemon under supervision or manage the privileged machine guard. Most users should use pb service to install, start, stop, and inspect the background process.

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
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal
* [pb daemon machine-guard](pb_daemon_machine-guard.md)	 - Manage protected local machine access
* [pb daemon run](pb_daemon_run.md)	 - Run Paperboat daemon under service supervision

