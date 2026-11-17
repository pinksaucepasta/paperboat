## pb tunnel doctor

Diagnose tunnel health

### Synopsis

Diagnose where a tunnel is failing: placement, the machine's connection, the edge and the
origin. --bundle prints a preview of a bounded support bundle for the absolute path you give,
and writes nothing; add --write-bundle to write exactly the previewed bundle. --bundle-max-bytes
bounds its size (64KiB to 8MiB, default 2MiB).

JSON output is supported with --json.

```
pb tunnel doctor <tunnel> [flags]
```

### Examples

```
  pb tunnel doctor web
  pb tunnel doctor web --bundle /tmp/bundle.json
  pb tunnel doctor web --bundle /tmp/bundle.json --write-bundle
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
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb tunnel](pb_tunnel.md)	 - Manage durable tunnels or start an ephemeral tunnel

