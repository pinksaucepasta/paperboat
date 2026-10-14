## pb config local-access proxy unset

Restore the default reverse-proxy port

### Synopsis

Remove the machine reverse-proxy override and restore automatic app-name routing through authorized port 80. Numeric URLs and explicit aliases retain their destinations. The running gateway is updated before success is reported.

JSON output is supported with --json.

```
pb config local-access proxy unset <machine-alias> [flags]
```

### Options

```
  -h, --help   help for unset
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

