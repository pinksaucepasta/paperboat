## pb domain show

Show a domain's DNS records, status and uses

### Synopsis

Show a domain's DNS records, status and uses

Show one domain's DNS records, verification and certificate state, the exact fix for a failed check, and the hostnames that use it.

JSON output is supported with --json.

```
pb domain show <domain> [flags]
```

### Options

```
  -h, --help   help for show
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

* [pb domain](pb_domain.md)	 - Add your own domains to the workspace

