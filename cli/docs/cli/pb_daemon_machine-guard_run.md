## pb daemon machine-guard run

Run the machine guard under service supervision

### Synopsis

Run the machine guard under service supervision

Start the foreground machine-guard process for its supervisor. This entrypoint is used by the installed root-owned service and normally should not be run from an interactive shell.

The machine guard is a privileged local helper for protected loopback listeners and trusted local HTTPS. Installation handles certificate trust; public DNS supplies the browser names. Use the dedicated service commands for the ordinary Paperboat daemon.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb daemon machine-guard run [flags]
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
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb daemon machine-guard](pb_daemon_machine-guard.md)	 - Manage protected local machine access

