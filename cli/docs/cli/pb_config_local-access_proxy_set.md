## pb config local-access proxy set

Set the machine reverse-proxy port

### Synopsis

Set the port used by otherwise-unmapped app names on this machine. Port 80 is the automatic default. The selected port must already be authorized; numeric URLs and explicit aliases retain their destinations.

JSON output is supported with --json.

```
pb config local-access proxy set <machine-alias> [port] [flags]
```

### Options

```
  -h, --help   help for set
      --json   print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb config local-access proxy](pb_config_local-access_proxy.md)	 - Route machine app names through Coolify or another reverse proxy

