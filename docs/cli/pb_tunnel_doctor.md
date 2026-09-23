## pb tunnel doctor

Diagnose tunnel health

### Synopsis

Diagnose tunnel health

JSON output is supported with --json.

```
pb tunnel doctor <tunnel> [flags]
```

### Options

```
      --bundle string          absolute path for a local support-bundle preview
      --bundle-max-bytes int   maximum bundle bytes (64KiB-8MiB) (default 2097152)
  -h, --help                   help for doctor
      --json                   print canonical JSON
      --write-bundle           write the exact previewed bundle (requires --bundle)
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --ephemeral          use the temporary preview lifecycle
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb tunnel](pb_tunnel.md)	 - Manage durable tunnels or start an ephemeral tunnel

