## pb daemon run

Run Paperboat daemon under service supervision

### Synopsis

Run Paperboat daemon under service supervision

Run the endpoint daemon in the foreground for service supervision. It remains active until stopped and uses the selected local configuration and server; use pb service for ordinary lifecycle control.

The daemon owns this machine's background connectivity and local API. Normal lifecycle management uses pb service; daemon run is the foreground entrypoint used by service supervision. Its machine guard subcommands manage protected local machine-name access.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb daemon run [flags]
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

* [pb daemon](pb_daemon.md)	 - Paperboat endpoint daemon

